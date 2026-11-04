package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// ClaimKey identifies a claim series for set and prune operations.
type ClaimKey struct {
	Kind      string
	Namespace string
	Name      string
}

func (k ClaimKey) labelValues() []string {
	return []string{k.Kind, k.Namespace, k.Name}
}

func (k ClaimKey) id() string {
	return k.Kind + "/" + k.Namespace + "/" + k.Name
}

var (
	claimMutex sync.Mutex
	claimKeys  = map[string]ClaimKey{}

	claimProvisioned = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "dbclaim_provisioned",
			Help: "Whether a DatabaseClaim or SchemaClaim is provisioned",
		},
		[]string{"kind", "namespace", "name"},
	)
)

func init() {
	ctrlmetrics.Registry.MustRegister(claimProvisioned)
}

func SetClaimProvisioned(kind, namespace, name string, provisioned bool) {
	key := ClaimKey{Kind: kind, Namespace: namespace, Name: name}

	claimMutex.Lock()
	defer claimMutex.Unlock()

	claimKeys[key.id()] = key
	value := 0.0
	if provisioned {
		value = 1.0
	}
	claimProvisioned.WithLabelValues(key.labelValues()...).Set(value)
}

// DeleteClaimProvisioned removes one claim's series. Claim cleanup calls this
// so metrics follow claim deletion even when DatabaseService is absent and
// cannot prune.
func DeleteClaimProvisioned(kind, namespace, name string) {
	key := ClaimKey{Kind: kind, Namespace: namespace, Name: name}

	claimMutex.Lock()
	defer claimMutex.Unlock()

	delete(claimKeys, key.id())
	claimProvisioned.DeleteLabelValues(key.labelValues()...)
}

func PruneClaimMetrics(currentClaims []ClaimKey) {
	current := make(map[string]struct{}, len(currentClaims))
	for _, claim := range currentClaims {
		current[claim.id()] = struct{}{}
	}

	claimMutex.Lock()
	defer claimMutex.Unlock()

	for id, key := range claimKeys {
		if _, exists := current[id]; exists {
			continue
		}

		delete(claimKeys, id)
		claimProvisioned.DeleteLabelValues(key.labelValues()...)
	}
}

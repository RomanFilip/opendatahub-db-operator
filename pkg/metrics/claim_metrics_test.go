package metrics

import (
	"testing"

	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestClaimMetrics(t *testing.T) {
	t.Run("updates provisioned state", func(t *testing.T) {
		g := NewWithT(t)

		claimProvisioned.Reset()
		SetClaimProvisioned("SchemaClaim", "team-a", "my-app", true)
		g.Expect(testutil.ToFloat64(claimProvisioned.WithLabelValues("SchemaClaim", "team-a", "my-app"))).To(Equal(1.0))

		SetClaimProvisioned("SchemaClaim", "team-a", "my-app", false)
		g.Expect(testutil.ToFloat64(claimProvisioned.WithLabelValues("SchemaClaim", "team-a", "my-app"))).To(Equal(0.0))
	})

	t.Run("tracks DatabaseClaim and SchemaClaim separately", func(t *testing.T) {
		g := NewWithT(t)

		claimProvisioned.Reset()
		SetClaimProvisioned("DatabaseClaim", "team-a", "my-app", true)
		SetClaimProvisioned("SchemaClaim", "team-a", "my-app", false)

		g.Expect(testutil.CollectAndCount(claimProvisioned)).To(Equal(2))
		g.Expect(testutil.ToFloat64(claimProvisioned.WithLabelValues("DatabaseClaim", "team-a", "my-app"))).To(Equal(1.0))
		g.Expect(testutil.ToFloat64(claimProvisioned.WithLabelValues("SchemaClaim", "team-a", "my-app"))).To(Equal(0.0))
	})

	t.Run("prunes metrics for deleted claims", func(t *testing.T) {
		g := NewWithT(t)

		claimProvisioned.Reset()
		SetClaimProvisioned("SchemaClaim", "team-a", "removed", false)
		SetClaimProvisioned("DatabaseClaim", "team-b", "current", true)

		PruneClaimMetrics([]ClaimKey{{
			Kind:      "DatabaseClaim",
			Namespace: "team-b",
			Name:      "current",
		}})

		g.Expect(testutil.CollectAndCount(claimProvisioned)).To(Equal(1))
		g.Expect(testutil.ToFloat64(claimProvisioned.WithLabelValues("DatabaseClaim", "team-b", "current"))).To(Equal(1.0))
	})

	t.Run("deletes a single claim series", func(t *testing.T) {
		g := NewWithT(t)

		claimProvisioned.Reset()
		SetClaimProvisioned("SchemaClaim", "team-a", "gone", true)
		SetClaimProvisioned("DatabaseClaim", "team-b", "keep", false)

		DeleteClaimProvisioned("SchemaClaim", "team-a", "gone")

		g.Expect(testutil.CollectAndCount(claimProvisioned)).To(Equal(1))
		g.Expect(testutil.ToFloat64(claimProvisioned.WithLabelValues("DatabaseClaim", "team-b", "keep"))).To(Equal(0.0))
	})
}

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package claimsecret verifies Owns(Secret) drift healing (RHOAIENG-96275):
// deleting a claim's credentials Secret re-provisions it promptly through
// the watch, not the periodic requeue. The DatabaseClaim reconciler is
// registered with a fake PostgreSQL client so provisioning completes without
// a database; the provider controller is intentionally absent so its
// reachability loop cannot overwrite the synthetic Reachable status.
package claimsecret

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	. "github.com/onsi/gomega"
	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	"github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	infraApi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
	"github.com/opendatahub-io/opendatahub-db-operator/internal/controller/databaseclaim"
	moduleconfig "github.com/opendatahub-io/opendatahub-db-operator/pkg/config"
	modulemanager "github.com/opendatahub-io/opendatahub-db-operator/pkg/manager"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/postgres"
	resourcelabels "github.com/opendatahub-io/opendatahub-db-operator/pkg/resources/labels"
	"github.com/opendatahub-io/opendatahub-db-operator/test/support"
)

// fakePostgres implements postgres.Client well enough for DatabaseClaim
// provisioning. State persists across reconciles so drift healing takes the
// rotation branch (role exists, Secret lost).
type fakePostgres struct {
	mu        sync.Mutex
	databases map[string]bool
	roles     map[string]bool
}

func (f *fakePostgres) Config() postgres.Config {
	return postgres.Config{Host: "rhai-db.database-system.svc", Port: 5432, User: "postgres", DBName: "postgres"}
}

func (f *fakePostgres) Close()                     {}
func (f *fakePostgres) Ping(context.Context) error { return nil }

func (f *fakePostgres) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.Contains(sql, "CREATE DATABASE"):
		f.databases[quotedTail(sql)] = true
	case strings.Contains(sql, "CREATE ROLE"):
		f.roles[quotedHead(sql)] = true
	}
	return pgconn.CommandTag{}, nil
}

func (f *fakePostgres) Query(context.Context, string, ...any) (postgres.Rows, error) {
	return nil, fmt.Errorf("unexpected Query")
}

func (f *fakePostgres) QueryRow(_ context.Context, sql string, args ...any) (pgx.Row, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.Contains(sql, "pg_database"):
		return boolRow{f.databases[args[0].(string)]}, nil
	case strings.Contains(sql, "pg_roles"):
		return boolRow{f.roles[args[0].(string)]}, nil
	default:
		return boolRow{false}, nil
	}
}

type boolRow struct{ v bool }

func (r boolRow) Scan(dest ...any) error {
	ptr, ok := dest[0].(*bool)
	if !ok {
		return fmt.Errorf("expected *bool")
	}
	*ptr = r.v
	return nil
}

func quotedHead(sql string) string {
	start := strings.Index(sql, `"`)
	if start < 0 {
		return ""
	}
	end := strings.Index(sql[start+1:], `"`)
	if end < 0 {
		return ""
	}
	return sql[start+1 : start+1+end]
}

func quotedTail(sql string) string {
	last := strings.LastIndex(sql, `"`)
	if last < 0 {
		return ""
	}
	prev := strings.LastIndex(sql[:last], `"`)
	if prev < 0 {
		return ""
	}
	return sql[prev+1 : last]
}

func TestClaimSecretDriftHealing(t *testing.T) {
	ctrl.SetLogger(logr.Discard())

	gomegaCfg := support.LoadGomegaConfig()
	SetDefaultEventuallyTimeout(gomegaCfg.EventuallyTimeout)
	SetDefaultEventuallyPollingInterval(gomegaCfg.EventuallyPollingInterval)
	g := NewWithT(t)

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	restCfg, err := env.Start()
	g.Expect(err).NotTo(HaveOccurred(),
		"starting envtest -- KUBEBUILDER_ASSETS must point at a directory with etcd/kube-apiserver "+
			"(see `setup-envtest use -p path`); `make test-envtest` sets this up automatically")
	t.Cleanup(func() {
		g.Expect(env.Stop()).To(Succeed())
	})

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	cfg, err := moduleconfig.Load()
	g.Expect(err).NotTo(HaveOccurred())
	// Keep the claim's periodic requeue longer than Eventually so a pass
	// proves the Owns(Secret) watch fired.
	cfg.DatabaseClaim.RetryInterval = gomegaCfg.EventuallyTimeout * 2

	scheme, err := modulemanager.NewScheme()
	g.Expect(err).NotTo(HaveOccurred())

	// The manager mirrors the operator: scoped Secret cache, Secret reads
	// bypassing the cache.
	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:    scheme,
		Cache:     modulemanager.SecretScopedCache(),
		NewClient: modulemanager.NewClientWithUncachedSecrets,
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
	})
	g.Expect(err).NotTo(HaveOccurred())

	fakePG := &fakePostgres{databases: map[string]bool{"postgres": true}, roles: map[string]bool{}}
	g.Expect(databaseclaim.NewReconciler(ctx, mgr, cfg, databaseclaim.Options{
		Recorder: mgr.GetEventRecorder(infraApi.DatabaseClaimResource),
		PostgresClientFactory: func(_ context.Context, _ postgres.Config) (postgres.Client, error) {
			return fakePG, nil
		},
	})).To(Succeed())

	managerDone := make(chan struct{})
	var managerErr error
	go func() {
		managerErr = mgr.Start(ctx)
		close(managerDone)
	}()
	t.Cleanup(func() {
		cancel()
		<-managerDone
		if managerErr != nil {
			t.Errorf("manager failed while stopping: %v", managerErr)
		}
	})
	g.Expect(mgr.GetCache().WaitForCacheSync(ctx)).To(BeTrue())

	cli := mgr.GetClient()

	for _, name := range []string{"database-system", "drift-workload"} {
		err := cli.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
		if err != nil {
			g.Expect(apierrors.IsAlreadyExists(err)).To(BeTrue(), "creating namespace %s: %v", name, err)
		}
	}

	// The admin Secret carries no managed label, exercising the claim path's
	// uncached read of administrator-managed Secrets.
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rhai-db-admin", Namespace: "database-system"},
		Data: map[string][]byte{
			"host":     []byte("rhai-db.database-system.svc"),
			"port":     []byte("5432"),
			"user":     []byte("postgres"),
			"password": []byte("admin-password"),
			"dbname":   []byte("postgres"),
		},
	}
	g.Expect(cli.Create(ctx, adminSecret)).To(Succeed())

	provider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "rhai-db"},
		Spec: infraApi.DatabaseProviderSpec{
			Type: infraApi.ProviderTypeInternal,
			Internal: &infraApi.InternalProviderSpec{
				Namespace: "database-system",
				Storage:   infraApi.StorageSpec{Size: resource.MustParse("1Gi")},
			},
		},
	}
	g.Expect(cli.Create(ctx, provider)).To(Succeed())
	provider.Status.Conditions = []fwapi.Condition{{
		Type:               "Reachable",
		Status:             metav1.ConditionTrue,
		Reason:             "Synthetic",
		LastTransitionTime: metav1.Now(),
	}}
	g.Expect(cli.Status().Update(ctx, provider)).To(Succeed())

	claim := &infraApi.DatabaseClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "analytics", Namespace: "drift-workload"},
		Spec: infraApi.DatabaseClaimSpec{
			Provider: infraApi.ProviderRef{Name: "rhai-db"},
			Database: "analytics",
		},
	}
	g.Expect(cli.Create(ctx, claim)).To(Succeed())

	secretKey := client.ObjectKey{Namespace: "drift-workload", Name: "analytics"}

	readSecret := func(g Gomega) *corev1.Secret {
		secret := &corev1.Secret{}
		g.Expect(cli.Get(ctx, secretKey, secret)).To(Succeed())
		return secret
	}

	g.Eventually(func(g Gomega) {
		secret := readSecret(g)
		g.Expect(secret.Labels).To(HaveKeyWithValue(resourcelabels.ManagedBy, resourcelabels.ManagedByValue))
		owner := metav1.GetControllerOf(secret)
		g.Expect(owner).NotTo(BeNil(), "credentials Secret must carry the claim's controller ownerRef")
		g.Expect(owner.Kind).To(Equal(infraApi.DatabaseClaimKind))
		current := &infraApi.DatabaseClaim{}
		g.Expect(cli.Get(ctx, client.ObjectKeyFromObject(claim), current)).To(Succeed())
		provisioned := conditions.FindStatusCondition(current, "Provisioned")
		g.Expect(provisioned).NotTo(BeNil())
		g.Expect(provisioned.Status).To(Equal(metav1.ConditionTrue), "%s", provisioned.Message)
	}).Should(Succeed(), "claim should provision and own its credentials Secret")

	originalPassword := string(readSecret(g).Data["password"])

	g.Expect(cli.Delete(ctx, readSecret(g))).To(Succeed())

	g.Eventually(func(g Gomega) {
		secret := readSecret(g)
		g.Expect(secret.Labels).To(HaveKeyWithValue(resourcelabels.ManagedBy, resourcelabels.ManagedByValue))
		owner := metav1.GetControllerOf(secret)
		g.Expect(owner).NotTo(BeNil())
		g.Expect(string(secret.Data["password"])).NotTo(BeEmpty())
		g.Expect(string(secret.Data["password"])).NotTo(Equal(originalPassword),
			"a deleted Secret is healed by rotating, since the password is unrecoverable")
	}).Should(Succeed(), "deleted credentials Secret should be re-provisioned promptly by the watch")

	rotatedPassword := string(readSecret(g).Data["password"])

	// A tampered password fails stamp verification and heals by rotating.
	tampered := readSecret(g).DeepCopy()
	tampered.Data["password"] = []byte("tampered-value")
	g.Expect(cli.Update(ctx, tampered)).To(Succeed())
	g.Eventually(func(g Gomega) {
		secret := readSecret(g)
		password := string(secret.Data["password"])
		g.Expect(password).NotTo(Equal("tampered-value"),
			"an unverifiable password must never be kept")
		g.Expect(password).NotTo(Equal(rotatedPassword),
			"a tampered password heals by rotating")
	}).Should(Succeed(), "tampered password should be rotated promptly by the watch")

	// A stripped controller ownerRef heals by re-adoption: the credential
	// stamp still verifies the content, so the claim preserves the password
	// and provisionAction re-stamps the ownerRef.
	stampedPassword := string(readSecret(g).Data["password"])
	stripped := readSecret(g).DeepCopy()
	stripped.OwnerReferences = nil
	g.Expect(cli.Update(ctx, stripped)).To(Succeed())
	g.Eventually(func(g Gomega) {
		secret := readSecret(g)
		owner := metav1.GetControllerOf(secret)
		g.Expect(owner).NotTo(BeNil(), "the controller ownerRef should be re-adopted")
		g.Expect(owner.Kind).To(Equal(infraApi.DatabaseClaimKind))
		g.Expect(string(secret.Data["password"])).To(Equal(stampedPassword),
			"a stamp-verified password must be preserved through re-adoption")
	}).Should(Succeed(), "stripped ownerRef should be healed promptly by the watch")
}

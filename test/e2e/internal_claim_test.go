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

package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	"github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
	infraapi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
	dbcontroller "github.com/opendatahub-io/opendatahub-db-operator/pkg/controller"
	modulemanager "github.com/opendatahub-io/opendatahub-db-operator/pkg/manager"
	"github.com/opendatahub-io/opendatahub-db-operator/test/support"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestInternalProviderClaimLifecycle(t *testing.T) {
	profile := os.Getenv("ODH_E2E_CERT_MANAGER_PROFILE")
	if profile == "" {
		t.Skip("set ODH_E2E_CERT_MANAGER_PROFILE to present or absent")
	}
	if profile != certManagerProfilePresent && profile != certManagerProfileAbsent {
		t.Fatalf("unknown cert-manager profile %q", profile)
	}
	tlsEnabled := profile == certManagerProfilePresent

	g := NewWithT(t)
	restCfg, err := ctrl.GetConfig()
	g.Expect(err).NotTo(HaveOccurred())
	scheme, err := modulemanager.NewScheme()
	g.Expect(err).NotTo(HaveOccurred())
	cli, err := client.New(restCfg, client.Options{Scheme: scheme})
	g.Expect(err).NotTo(HaveOccurred())
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	t.Cleanup(cancel)

	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "odh-db-internal-"}}
	g.Expect(cli.Create(ctx, namespace)).To(Succeed())
	t.Cleanup(func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		if err := support.DeleteNamespace(cleanupCtx, cli, namespace.Name, namespace.UID); err != nil {
			t.Errorf("cleaning consumer namespace: %v", err)
		}
	})

	provider := &infraapi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "odh-db-internal-"},
		Spec: infraapi.DatabaseProviderSpec{
			Type: infraapi.ProviderTypeInternal,
			Internal: &infraapi.InternalProviderSpec{
				Storage: infraapi.StorageSpec{Size: resource.MustParse("1Gi")},
			},
		},
	}
	if tlsEnabled {
		provider.Spec.Internal.TLS = &infraapi.InternalProviderTLSSpec{}
	}
	g.Expect(cli.Create(ctx, provider)).To(Succeed())
	t.Cleanup(func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		err := cli.Delete(cleanupCtx, provider, client.Preconditions{UID: &provider.UID})
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			t.Errorf("cleaning internal provider: %v", err)
		}
	})

	g.Eventually(func(g Gomega) {
		current := &infraapi.DatabaseProvider{}
		g.Expect(cli.Get(ctx, client.ObjectKeyFromObject(provider), current)).To(Succeed())
		condition := conditions.FindStatusCondition(current, "Reachable")
		g.Expect(condition).NotTo(BeNil())
		g.Expect(condition.Status).To(Equal(metav1.ConditionTrue), "%s", condition.Message)
		limit := conditions.FindStatusCondition(current, infraapi.ConditionOperationalLimitations)
		g.Expect(limit).NotTo(BeNil())
		g.Expect(limit.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(limit.Severity).To(Equal(fwapi.ConditionSeverityInfo))
		ready := conditions.FindStatusCondition(current, "Ready")
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(current.Status.Connection.Host).NotTo(BeEmpty())
		g.Expect(current.Status.TLS).NotTo(BeNil())
		g.Expect(current.Status.TLS.Enabled).To(Equal(tlsEnabled))
		g.Expect(current.Status.TLS.Ready).To(Equal(tlsEnabled))
	}).WithTimeout(5*time.Minute).Should(Succeed(), "internal provider did not become reachable")

	claim := &infraapi.DatabaseClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "connection-contract", Namespace: namespace.Name},
		Spec: infraapi.DatabaseClaimSpec{
			Provider: infraapi.ProviderRef{Name: provider.Name},
			Access:   infraapi.AccessModeReadWrite,
		},
	}
	g.Expect(cli.Create(ctx, claim)).To(Succeed())
	t.Cleanup(func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		err := cli.Delete(cleanupCtx, claim, client.Preconditions{UID: &claim.UID})
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			t.Errorf("cleaning database claim: %v", err)
		}
	})

	g.Eventually(func(g Gomega) {
		current := &infraapi.DatabaseClaim{}
		g.Expect(cli.Get(ctx, client.ObjectKeyFromObject(claim), current)).To(Succeed())
		condition := conditions.FindStatusCondition(current, "Provisioned")
		g.Expect(condition).NotTo(BeNil())
		g.Expect(condition.Status).To(Equal(metav1.ConditionTrue), "%s", condition.Message)
		g.Expect(current.Status.Connection.SecretRef.Name).To(Equal(claim.Name))
	}).WithTimeout(3*time.Minute).Should(Succeed(), "internal database claim did not provision")

	credentials := &corev1.Secret{}
	g.Eventually(func(g Gomega) {
		g.Expect(cli.Get(ctx, client.ObjectKey{Namespace: namespace.Name, Name: claim.Name}, credentials)).To(Succeed())
	}).WithTimeout(time.Minute).Should(Succeed())
	for _, key := range []string{"host", "port", "user", "password", "dbname"} {
		g.Expect(credentials.Data[key]).NotTo(BeEmpty(), "claim Secret needs %q", key)
	}
	expectedSSLMode := "disable"
	if tlsEnabled {
		expectedSSLMode = "verify-full"
		g.Expect(credentials.Data["ca.crt"]).NotTo(BeEmpty())
	} else {
		g.Expect(credentials.Data).NotTo(HaveKey("ca.crt"))
	}
	g.Expect(string(credentials.Data["sslmode"])).To(Equal(expectedSSLMode))
	g.Expect(credentials.Annotations["opendatahub.io/connection-type-protocol"]).To(Equal("postgres"))

	image := envOrDefault("ODH_E2E_CONSUMER_IMAGE", "postgres:16")
	operatorNamespace := os.Getenv("ODH_E2E_NAMESPACE")
	g.Expect(operatorNamespace).NotTo(BeEmpty(), "ODH_E2E_NAMESPACE must name the operator namespace")
	internalAdminSecret := &corev1.Secret{}
	g.Expect(cli.Get(ctx, client.ObjectKey{
		Namespace: operatorNamespace,
		Name:      dbcontroller.InternalAdminSecretName(provider.Name),
	}, internalAdminSecret)).To(Succeed())
	adminSecretCopy := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "claim-contract-admin-", Namespace: namespace.Name},
		Type:       internalAdminSecret.Type,
		Data:       internalAdminSecret.Data,
	}
	g.Expect(cli.Create(ctx, adminSecretCopy)).To(Succeed())
	t.Cleanup(func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		err := cli.Delete(cleanupCtx, adminSecretCopy, client.Preconditions{UID: &adminSecretCopy.UID})
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			t.Errorf("cleaning temporary internal admin Secret: %v", err)
		}
	})
	createAdminContractTable(t, ctx, cli, restCfg, namespace.Name,
		adminSecretCopy.Name, string(credentials.Data["dbname"]),
		string(credentials.Data["user"]), expectedSSLMode, image, tlsEnabled)
	runConsumer(t, ctx, cli, restCfg, namespace.Name, claim.Name, image, tlsEnabled)

	g.Expect(cli.Delete(ctx, claim, client.Preconditions{UID: &claim.UID})).To(Succeed())
	g.Eventually(func(g Gomega) {
		current := &infraapi.DatabaseClaim{}
		err := cli.Get(ctx, client.ObjectKeyFromObject(claim), current)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "claim still exists: %v", err)
	}).WithTimeout(2 * time.Minute).Should(Succeed())
}

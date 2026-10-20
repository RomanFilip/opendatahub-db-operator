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
	"github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
	infraapi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
	modulemanager "github.com/opendatahub-io/opendatahub-db-operator/pkg/manager"
	"github.com/opendatahub-io/opendatahub-db-operator/test/support"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestExternalProviderClaimLifecycle(t *testing.T) {
	adminNamespace := os.Getenv("ODH_E2E_EXTERNAL_ADMIN_SECRET_NAMESPACE")
	adminName := os.Getenv("ODH_E2E_EXTERNAL_ADMIN_SECRET_NAME")
	if adminNamespace == "" && adminName == "" {
		t.Skip("set ODH_E2E_EXTERNAL_ADMIN_SECRET_NAMESPACE and ODH_E2E_EXTERNAL_ADMIN_SECRET_NAME")
	}
	if adminNamespace == "" || adminName == "" {
		t.Fatal("both external admin Secret namespace and name are required")
	}

	g := NewWithT(t)
	restCfg, err := ctrl.GetConfig()
	g.Expect(err).NotTo(HaveOccurred())
	scheme, err := modulemanager.NewScheme()
	g.Expect(err).NotTo(HaveOccurred())
	cli, err := client.New(restCfg, client.Options{Scheme: scheme})
	g.Expect(err).NotTo(HaveOccurred())
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	t.Cleanup(cancel)

	adminSecret := &corev1.Secret{}
	g.Expect(cli.Get(ctx, client.ObjectKey{Namespace: adminNamespace, Name: adminName}, adminSecret)).To(Succeed())
	for _, key := range []string{"host", "port", "user", "password", "dbname", "ca.crt"} {
		g.Expect(adminSecret.Data[key]).NotTo(BeEmpty(), "external admin Secret needs %q", key)
	}
	g.Expect(string(adminSecret.Data["sslmode"])).To(Equal("verify-full"),
		"the administrator-supplied target must support hostname-verified TLS")

	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "odh-db-contract-"}}
	g.Expect(cli.Create(ctx, namespace)).To(Succeed())
	t.Cleanup(func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		if err := support.DeleteNamespace(cleanupCtx, cli, namespace.Name, namespace.UID); err != nil {
			t.Errorf("cleaning consumer namespace: %v", err)
		}
	})

	provider := &infraapi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "odh-db-e2e-external-"},
		Spec: infraapi.DatabaseProviderSpec{
			Type: infraapi.ProviderTypeExternal,
			External: &infraapi.ExternalProviderSpec{
				ConnectionSecretRef: corev1.SecretReference{Namespace: adminNamespace, Name: adminName},
			},
		},
	}
	g.Expect(cli.Create(ctx, provider)).To(Succeed())
	t.Cleanup(func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		err := cli.Delete(cleanupCtx, provider, client.Preconditions{UID: &provider.UID})
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			t.Errorf("cleaning external provider: %v", err)
		}
	})

	g.Eventually(func(g Gomega) {
		current := &infraapi.DatabaseProvider{}
		g.Expect(cli.Get(ctx, client.ObjectKeyFromObject(provider), current)).To(Succeed())
		condition := conditions.FindStatusCondition(current, "Reachable")
		g.Expect(condition).NotTo(BeNil())
		g.Expect(condition.Status).To(Equal(metav1.ConditionTrue), "%s", condition.Message)
		g.Expect(current.Status.TLS).NotTo(BeNil())
		g.Expect(current.Status.TLS.Ready).To(BeTrue())
	}).WithTimeout(2*time.Minute).Should(Succeed(), "external provider did not become reachable")

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
	}).WithTimeout(2*time.Minute).Should(Succeed(), "database claim did not provision")

	credentials := &corev1.Secret{}
	g.Eventually(func(g Gomega) {
		g.Expect(cli.Get(ctx, client.ObjectKey{Namespace: namespace.Name, Name: claim.Name}, credentials)).To(Succeed())
	}).WithTimeout(time.Minute).Should(Succeed())
	for _, key := range []string{"host", "port", "user", "password", "dbname", "ca.crt"} {
		g.Expect(credentials.Data[key]).NotTo(BeEmpty(), "claim Secret needs %q", key)
	}
	g.Expect(string(credentials.Data["sslmode"])).To(Equal("verify-full"))
	g.Expect(credentials.Annotations["opendatahub.io/connection-type-protocol"]).To(Equal("postgres"))

	image := envOrDefault("ODH_E2E_CONSUMER_IMAGE", "postgres:16")
	createAdminContractTable(t, ctx, cli, restCfg, adminNamespace, adminName,
		string(credentials.Data["dbname"]), string(credentials.Data["user"]),
		string(adminSecret.Data["sslmode"]), image, true)
	runConsumer(t, ctx, cli, restCfg, namespace.Name, claim.Name, image, true)

	g.Expect(cli.Delete(ctx, claim, client.Preconditions{UID: &claim.UID})).To(Succeed())
	g.Eventually(func(g Gomega) {
		current := &infraapi.DatabaseClaim{}
		err := cli.Get(ctx, client.ObjectKeyFromObject(claim), current)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "claim still exists: %v", err)
	}).WithTimeout(2 * time.Minute).Should(Succeed())
}

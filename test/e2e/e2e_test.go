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

// Package e2e verifies the operator installed from config/chart against a real cluster.
package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	"github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
	"github.com/opendatahub-io/odh-platform-utilities/framework/controller/reconciler"
	infraapi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
	servicesv1alpha1 "github.com/opendatahub-io/opendatahub-db-operator/api/services/v1alpha1"
	moduleconfig "github.com/opendatahub-io/opendatahub-db-operator/pkg/config"
	dbcontroller "github.com/opendatahub-io/opendatahub-db-operator/pkg/controller"
	modulemanager "github.com/opendatahub-io/opendatahub-db-operator/pkg/manager"
	"github.com/opendatahub-io/opendatahub-db-operator/test/support"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const (
	defaultNamespace  = "opendatahub-db-operator-e2e"
	defaultRelease    = "opendatahub-db-operator-e2e"
	managerDeployment = "odh-db-operator-operator"
	managerBinding    = "odh-db-operator-manager-rolebinding"
	leaderBinding     = "odh-db-operator-leader-election-rolebinding"
)

func TestMain(m *testing.M) {
	gomegaCfg := support.LoadGomegaConfig()
	SetDefaultEventuallyTimeout(gomegaCfg.EventuallyTimeout)
	SetDefaultEventuallyPollingInterval(gomegaCfg.EventuallyPollingInterval)
	SetDefaultConsistentlyPollingInterval(gomegaCfg.ConsistentlyPollingInterval)

	m.Run()
}

// TestHelmInstalledManagerDefaultProviderLifecycle verifies the installed
// manager's default provider, providerless claim, and guarded shutdown.
func TestHelmInstalledManagerDefaultProviderLifecycle(t *testing.T) {
	g := NewWithT(t)

	namespace := envOrDefault("ODH_E2E_NAMESPACE", defaultNamespace)
	release := envOrDefault("ODH_E2E_RELEASE", defaultRelease)

	restCfg, err := ctrl.GetConfig()
	g.Expect(err).NotTo(HaveOccurred(),
		"no kubeconfig / current context -- this test needs the Helm-installed operator on a real cluster")

	scheme, err := modulemanager.NewScheme()
	g.Expect(err).NotTo(HaveOccurred())
	kubeClient, err := client.New(restCfg, client.Options{Scheme: scheme})
	g.Expect(err).NotTo(HaveOccurred())

	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	t.Cleanup(cancel)

	g.Expect(kubeClient.List(ctx, &servicesv1alpha1.DatabaseServiceList{})).To(Succeed(),
		"listing DatabaseService failed -- run `make test-e2e-setup` to install the Helm chart first")

	deploymentKey := types.NamespacedName{Namespace: namespace, Name: managerDeployment}
	deployment := &appsv1.Deployment{}
	g.Eventually(func(g Gomega) {
		g.Expect(kubeClient.Get(ctx, deploymentKey, deployment)).To(Succeed())
		g.Expect(deployment.Spec.Replicas).NotTo(BeNil())
		desiredReplicas := *deployment.Spec.Replicas
		g.Expect(desiredReplicas).To(BeNumerically(">", 0))
		g.Expect(deployment.Status.ObservedGeneration).To(BeNumerically(">=", deployment.Generation))
		g.Expect(deployment.Status.ReadyReplicas).To(BeNumerically(">=", desiredReplicas))
		g.Expect(deployment.Status.AvailableReplicas).To(BeNumerically(">=", desiredReplicas))
		available := false
		for _, condition := range deployment.Status.Conditions {
			if condition.Type == appsv1.DeploymentAvailable && condition.Status == corev1.ConditionTrue {
				available = true
				break
			}
		}
		g.Expect(available).To(BeTrue(), "Deployment has no Available=True condition")
	}).Should(Succeed(), "waiting for the Helm chart Deployment to become Ready")

	serviceAccountName := deployment.Spec.Template.Spec.ServiceAccountName
	g.Expect(serviceAccountName).NotTo(BeEmpty(), "chart Deployment has no ServiceAccount")
	serviceAccount := &corev1.ServiceAccount{}
	err = kubeClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: serviceAccountName}, serviceAccount)
	g.Expect(err).To(Succeed(), "chart Deployment ServiceAccount does not exist")

	managerRoleBinding := &rbacv1.ClusterRoleBinding{}
	g.Expect(kubeClient.Get(ctx, types.NamespacedName{Name: managerBinding}, managerRoleBinding)).To(Succeed(),
		"chart manager ClusterRoleBinding does not exist")
	g.Expect(managerRoleBinding.RoleRef.Kind).To(Equal("ClusterRole"))
	g.Expect(managerRoleBinding.RoleRef.Name).To(Equal("odh-db-operator-manager-role"))
	g.Expect(hasServiceAccountSubject(managerRoleBinding.Subjects, namespace, serviceAccountName)).To(BeTrue(),
		"chart manager ClusterRoleBinding does not grant permissions to the Deployment ServiceAccount")

	leaderElectionBinding := &rbacv1.RoleBinding{}
	err = kubeClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: leaderBinding}, leaderElectionBinding)
	g.Expect(err).To(Succeed(), "chart leader-election RoleBinding does not exist")
	g.Expect(hasServiceAccountSubject(leaderElectionBinding.Subjects, namespace, serviceAccountName)).To(BeTrue(),
		"chart leader-election RoleBinding does not grant permissions to the Deployment ServiceAccount")

	managerImage := ""
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name == "manager" {
			managerImage = container.Image
			break
		}
	}
	g.Expect(managerImage).NotTo(BeEmpty(), "chart Deployment has no manager container")
	if expectedImage := os.Getenv("ODH_E2E_OPERATOR_IMAGE"); expectedImage != "" {
		g.Expect(managerImage).To(Equal(expectedImage), "chart Deployment is not using the freshly built test image")
	}
	t.Logf("Helm release %q Deployment %s/%s is Ready; manager image=%q, ServiceAccount=%q; "+
		"chart manager and leader-election bindings reference that ServiceAccount",
		release, namespace, deployment.Name, managerImage, serviceAccountName)

	instance := &servicesv1alpha1.DatabaseService{
		ObjectMeta: metav1.ObjectMeta{Name: servicesv1alpha1.DatabaseServiceInstanceName},
	}
	g.Expect(kubeClient.Create(ctx, instance)).To(Succeed(),
		"creating the singleton DatabaseService; the e2e setup should remove leftovers before each run")
	createdUID := instance.UID
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cleanupCancel()

		deleteErr := kubeClient.Delete(cleanupCtx, &servicesv1alpha1.DatabaseService{
			ObjectMeta: metav1.ObjectMeta{Name: servicesv1alpha1.DatabaseServiceInstanceName, UID: createdUID},
		}, client.Preconditions{UID: &createdUID})
		if deleteErr != nil && !apierrors.IsNotFound(deleteErr) && !apierrors.IsConflict(deleteErr) {
			t.Errorf("cleaning up DatabaseService singleton: %v", deleteErr)
			return
		}

		g.Eventually(func(g Gomega) {
			current := &servicesv1alpha1.DatabaseService{}
			err := kubeClient.Get(cleanupCtx, client.ObjectKey{Name: servicesv1alpha1.DatabaseServiceInstanceName}, current)
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "DatabaseService singleton still exists after cleanup: %v", err)
		}).WithTimeout(3 * time.Minute).WithPolling(500 * time.Millisecond).Should(Succeed())
	})

	var reconciled *servicesv1alpha1.DatabaseService
	g.Eventually(func(g Gomega) {
		got := &servicesv1alpha1.DatabaseService{}
		g.Expect(kubeClient.Get(ctx, client.ObjectKeyFromObject(instance), got)).To(Succeed())
		g.Expect(got.Status.Phase).To(Equal(reconciler.DefaultPhaseReady))

		readyCondition := conditions.FindStatusCondition(got, string(fwapi.ConditionTypeReady))
		g.Expect(readyCondition).NotTo(BeNil(), "no Ready condition on status.conditions")
		g.Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(got.Status.ComponentReleaseStatus.GetRelease(moduleconfig.ReleasePlatform)).NotTo(BeNil(),
			"no platform release in status.releases")

		reconciled = got.DeepCopy()
	}).Should(Succeed(), "waiting for the chart-deployed manager to reconcile DatabaseService to Ready")

	reconciled.TypeMeta = metav1.TypeMeta{
		APIVersion: servicesv1alpha1.GroupVersion.String(),
		Kind:       "DatabaseService",
	}
	statusYAML, err := yaml.Marshal(reconciled)
	g.Expect(err).NotTo(HaveOccurred())
	t.Logf(
		"kubectl get databaseservice %s -o yaml equivalent:\n%s",
		servicesv1alpha1.DatabaseServiceInstanceName,
		statusYAML,
	)

	providerName := moduleconfig.DefaultProviderName
	g.Expect(reconciled.Status.DefaultProviderName).To(Equal(providerName))
	provider := &infraapi.DatabaseProvider{}
	g.Expect(kubeClient.Get(ctx, client.ObjectKey{Name: providerName}, provider)).To(Succeed())
	g.Expect(provider.Spec.Type).To(Equal(infraapi.ProviderTypeInternal))
	reachable := conditions.FindStatusCondition(provider, "Reachable")
	g.Expect(reachable).NotTo(BeNil())
	g.Expect(reachable.Status).To(Equal(metav1.ConditionTrue))
	statefulSetKey := client.ObjectKey{Namespace: namespace, Name: providerName}
	statefulSet := &appsv1.StatefulSet{}
	g.Expect(kubeClient.Get(ctx, statefulSetKey, statefulSet)).To(Succeed())
	g.Expect(statefulSet.Spec.Replicas).NotTo(BeNil())
	g.Expect(*statefulSet.Spec.Replicas).To(Equal(int32(1)))

	consumerNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "odh-db-default-"}}
	g.Expect(kubeClient.Create(ctx, consumerNamespace)).To(Succeed())
	t.Cleanup(func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		if err := support.DeleteNamespace(cleanupCtx, kubeClient, consumerNamespace.Name, consumerNamespace.UID); err != nil {
			t.Errorf("cleaning default-provider consumer namespace: %v", err)
		}
	})

	claim := &infraapi.DatabaseClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "default-database", Namespace: consumerNamespace.Name},
	}
	g.Expect(kubeClient.Create(ctx, claim)).To(Succeed())
	t.Cleanup(func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), 2*time.Minute)
		defer done()
		err := kubeClient.Delete(cleanupCtx, claim, client.Preconditions{UID: &claim.UID})
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			t.Errorf("cleaning default-provider claim: %v", err)
			return
		}
		g.Eventually(func() bool {
			err := kubeClient.Get(cleanupCtx, client.ObjectKeyFromObject(claim), &infraapi.DatabaseClaim{})
			return apierrors.IsNotFound(err)
		}).WithContext(cleanupCtx).Should(BeTrue())
	})

	g.Eventually(func(g Gomega) {
		current := &infraapi.DatabaseClaim{}
		g.Expect(kubeClient.Get(ctx, client.ObjectKeyFromObject(claim), current)).To(Succeed())
		g.Expect(current.Status.Provider).To(Equal(providerName))
		provisioned := conditions.FindStatusCondition(current, "Provisioned")
		g.Expect(provisioned).NotTo(BeNil())
		g.Expect(provisioned.Status).To(Equal(metav1.ConditionTrue), "%s", provisioned.Message)
	}).WithTimeout(3 * time.Minute).Should(Succeed())
	credentials := &corev1.Secret{}
	g.Eventually(func() error {
		return kubeClient.Get(ctx, client.ObjectKey{Namespace: consumerNamespace.Name, Name: claim.Name}, credentials)
	}).WithTimeout(time.Minute).Should(Succeed())
	for _, key := range []string{"host", "port", "user", "password", "dbname", "sslmode"} {
		g.Expect(credentials.Data[key]).NotTo(BeEmpty(), "claim Secret needs %q", key)
	}
	consumer := consumerPod(
		consumerNamespace.Name, claim.Name, envOrDefault("ODH_E2E_CONSUMER_IMAGE", "postgres:16"), false,
	)
	consumer.Spec.Containers[0].Command = []string{"/bin/sh", "-ec", connectionScript + `
psql -X -qAt -v ON_ERROR_STOP=1 "$connection" -c 'SELECT 1' | grep -qx 1
`}
	runPod(t, ctx, kubeClient, restCfg, consumer)

	g.Expect(kubeClient.Delete(ctx, instance, client.Preconditions{UID: &createdUID})).To(Succeed())
	g.Eventually(func(g Gomega) {
		current := &servicesv1alpha1.DatabaseService{}
		g.Expect(kubeClient.Get(ctx, client.ObjectKeyFromObject(instance), current)).To(Succeed())
		blocked := conditions.FindStatusCondition(current, "DefaultProviderReady")
		g.Expect(blocked).NotTo(BeNil())
		g.Expect(blocked.Reason).To(Equal("BlockingClaims"))
		g.Expect(blocked.Message).To(ContainSubstring(consumerNamespace.Name + "/" + claim.Name))
	}).WithTimeout(2 * time.Minute).Should(Succeed())
	g.Expect(kubeClient.Get(ctx, client.ObjectKey{Name: providerName}, &infraapi.DatabaseProvider{})).To(Succeed())

	g.Expect(kubeClient.Delete(ctx, claim, client.Preconditions{UID: &claim.UID})).To(Succeed())
	g.Eventually(func() bool {
		err := kubeClient.Get(ctx, client.ObjectKeyFromObject(claim), &infraapi.DatabaseClaim{})
		return apierrors.IsNotFound(err)
	}).WithTimeout(2 * time.Minute).Should(BeTrue())
	g.Eventually(func(g Gomega) {
		err := kubeClient.Get(ctx, client.ObjectKeyFromObject(instance), &servicesv1alpha1.DatabaseService{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "DatabaseService should finish deleting: %v", err)
		err = kubeClient.Get(ctx, client.ObjectKey{Name: providerName}, &infraapi.DatabaseProvider{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "default provider should be removed: %v", err)
		err = kubeClient.Get(ctx, statefulSetKey, &appsv1.StatefulSet{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "default workload should be removed: %v", err)
	}).WithTimeout(3 * time.Minute).Should(Succeed())
	for _, obj := range []client.Object{&corev1.PersistentVolumeClaim{}, &corev1.Secret{}} {
		key := client.ObjectKey{Namespace: namespace, Name: providerName}
		if _, ok := obj.(*corev1.Secret); ok {
			key.Name = dbcontroller.InternalAdminSecretName(providerName)
		}
		g.Expect(kubeClient.Get(ctx, key, obj)).To(Succeed())
		g.Expect(obj.GetOwnerReferences()).To(BeEmpty(), "%s must be retained", key)
	}
}

func hasServiceAccountSubject(subjects []rbacv1.Subject, namespace, name string) bool {
	for _, subject := range subjects {
		if subject.Kind == "ServiceAccount" && subject.Namespace == namespace && subject.Name == name {
			return true
		}
	}

	return false
}

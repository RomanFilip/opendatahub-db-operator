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
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infraapi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
	dbcontroller "github.com/opendatahub-io/opendatahub-db-operator/pkg/controller"
	modulemanager "github.com/opendatahub-io/opendatahub-db-operator/pkg/manager"
	"github.com/opendatahub-io/opendatahub-db-operator/test/support"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const baselinePolicyName = "odh-db-operator-e2e-default-deny"

// TestOperatorRunsUnderDefaultDenyBaseline verifies AC2 of RHOAIENG-96275 in
// two phases.
//
// Phase one (every cluster, every CI run): the chart ships the operator
// NetworkPolicy with the expected shape -- the allowances that keep the
// operator working when a default-deny baseline isolates its namespace.
//
// Phase two (only where the CNI enforces NetworkPolicy): a deny-all baseline
// is applied to the operator namespace and the manager is restarted inside
// it, proving readiness, leader election, informers, and PostgreSQL
// provisioning under deny-all networking. The phase is gated by a runtime
// enforcement probe rather than an environment variable, so it cannot be
// forgotten, and it can never pass vacuously on a cluster whose CNI ignores
// NetworkPolicy: the probe skips with a reason instead. See CONTRIBUTING's
// test-environments section for the cluster requirements.
func TestOperatorRunsUnderDefaultDenyBaseline(t *testing.T) {
	g := NewWithT(t)

	restCfg, err := ctrl.GetConfig()
	g.Expect(err).NotTo(HaveOccurred(),
		"no kubeconfig / current context -- this test needs the Helm-installed operator on a real cluster")
	scheme, err := modulemanager.NewScheme()
	g.Expect(err).NotTo(HaveOccurred())
	cli, err := client.New(restCfg, client.Options{Scheme: scheme})
	g.Expect(err).NotTo(HaveOccurred())

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
	t.Cleanup(cancel)

	namespace := envOrDefault("ODH_E2E_NAMESPACE", defaultNamespace)
	image := envOrDefault("ODH_E2E_CONSUMER_IMAGE", "postgres:16")

	// Phase one: the chart must ship the operator NetworkPolicy; it is what
	// admits the flows the baseline would otherwise deny. These assertions
	// are meaningful on any cluster, including CI's kind.
	operatorPolicy := &networkingv1.NetworkPolicy{}
	g.Expect(cli.Get(ctx, types.NamespacedName{
		Namespace: namespace,
		Name:      "odh-db-operator-controller-manager",
	}, operatorPolicy)).To(Succeed(), "the chart must deploy the operator NetworkPolicy")
	g.Expect(operatorPolicy.Spec.PolicyTypes).To(ContainElement(networkingv1.PolicyTypeIngress))
	g.Expect(operatorPolicy.Spec.PolicyTypes).To(ContainElement(networkingv1.PolicyTypeEgress))
	g.Expect(operatorPolicy.Spec.Egress).To(HaveLen(1), "egress must be the allow-all contract stopgap")

	// Phase two gate: prove this cluster enforces NetworkPolicy before
	// asserting anything under deny-all networking.
	if !networkPolicyEnforced(t, ctx, cli, image) {
		t.Skipf("NetworkPolicy is not enforced on this cluster (its CNI ignores policies, " +
			"e.g. kind's default kindnet) -- the default-deny baseline scenario would pass " +
			"vacuously here. Run on a cluster with an enforcing CNI (OpenShift/OVN-K, " +
			"Calico, Cilium); see CONTRIBUTING's test-environments section")
	}

	// Apply the default-deny baseline: deny all ingress and egress for every
	// pod in the operator namespace, mirroring what the ODH platform operator
	// imposes on module namespaces. The operator stays reachable-and-working
	// only through the allowances its own NetworkPolicy adds.
	baseline := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: baselinePolicyName, Namespace: namespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
	g.Expect(cli.Create(ctx, baseline)).To(Succeed())

	// Claim and provider teardown depend on the operator reconciling
	// finalizers, which must not happen under deny-all networking, so the
	// baseline is removed at the end of a successful run — before the
	// t.Cleanup stack runs. This registration is the fallback for failure
	// paths, where teardown noise is bounded but expected.
	removeBaseline := func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		if err := cli.Delete(cleanupCtx, baseline); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("removing default-deny baseline: %v", err)
		}
	}
	t.Cleanup(removeBaseline)

	// Restart the manager inside the baseline. Coming back Ready proves the
	// API server is reachable (leader election and informers require it);
	// staying Ready proves probes pass.
	deploymentKey := types.NamespacedName{Namespace: namespace, Name: managerDeployment}
	restart := time.Now().UTC().Format(time.RFC3339)
	deployment := &appsv1.Deployment{}
	g.Expect(cli.Get(ctx, deploymentKey, deployment)).To(Succeed())
	if deployment.Spec.Template.Annotations == nil {
		deployment.Spec.Template.Annotations = map[string]string{}
	}
	deployment.Spec.Template.Annotations["e2e.opendatahub.io/restarted-at"] = restart
	g.Expect(cli.Update(ctx, deployment)).To(Succeed())

	g.Eventually(func(g Gomega) {
		current := &appsv1.Deployment{}
		g.Expect(cli.Get(ctx, deploymentKey, current)).To(Succeed())
		// The controller must have observed the restart generation first:
		// the Progressing condition and ReadyReplicas left over from the
		// previous rollout satisfy the checks below before the controller
		// has seen the update, which would let the pre-restart pod -- whose
		// connections predate the baseline -- serve every assertion.
		g.Expect(current.Status.ObservedGeneration).To(BeNumerically(">=", current.Generation),
			"deployment controller has not observed the restarted generation")
		// Progressing=True with reason NewReplicaSetAvailable is then the
		// rollout-complete verdict: the restarted pod is Ready and the
		// pre-restart pod is terminated.
		progressing := false
		for _, condition := range current.Status.Conditions {
			if condition.Type == appsv1.DeploymentProgressing {
				progressing = true
				g.Expect(condition.Status).To(Equal(corev1.ConditionTrue))
				g.Expect(condition.Reason).To(Equal("NewReplicaSetAvailable"),
					"rollout must complete before asserting behavior under the baseline")
			}
		}
		g.Expect(progressing).To(BeTrue(), "deployment has no Progressing condition")
		g.Expect(current.Status.UpdatedReplicas).To(BeNumerically(">=", 1))
		g.Expect(current.Status.ReadyReplicas).To(BeNumerically(">=", 1),
			"manager must become Ready under the default-deny baseline (API server unreachable otherwise)")
	}).WithTimeout(5*time.Minute).Should(Succeed(), "manager did not restart Ready under the baseline")

	// Prove PostgreSQL reachability under the baseline: an Internal provider
	// becomes Reachable and a claim provisions end to end.
	consumerNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "odh-db-baseline-"}}
	g.Expect(cli.Create(ctx, consumerNamespace)).To(Succeed())
	t.Cleanup(func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		if err := support.DeleteNamespace(cleanupCtx, cli, consumerNamespace.Name, consumerNamespace.UID); err != nil {
			t.Errorf("cleaning consumer namespace: %v", err)
		}
	})

	provider := &infraapi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "odh-db-baseline-"},
		Spec: infraapi.DatabaseProviderSpec{
			Type:     infraapi.ProviderTypeInternal,
			Internal: &infraapi.InternalProviderSpec{Storage: infraapi.StorageSpec{Size: resource.MustParse("1Gi")}},
		},
	}
	g.Expect(cli.Create(ctx, provider)).To(Succeed())
	t.Cleanup(func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		err := cli.Delete(cleanupCtx, provider, client.Preconditions{UID: &provider.UID})
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			t.Errorf("cleaning baseline provider: %v", err)
		}
	})

	g.Eventually(func(g Gomega) {
		current := &infraapi.DatabaseProvider{}
		g.Expect(cli.Get(ctx, client.ObjectKeyFromObject(provider), current)).To(Succeed())
		condition := conditions.FindStatusCondition(current, "Reachable")
		g.Expect(condition).NotTo(BeNil())
		g.Expect(condition.Status).To(Equal(metav1.ConditionTrue),
			"provider must reach PostgreSQL under the baseline: %s", condition.Message)
	}).WithTimeout(5*time.Minute).Should(Succeed(), "provider did not become reachable under the baseline")

	claim := &infraapi.DatabaseClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "baseline-connection", Namespace: consumerNamespace.Name},
		Spec:       infraapi.DatabaseClaimSpec{Provider: infraapi.ProviderRef{Name: provider.Name}},
	}
	g.Expect(cli.Create(ctx, claim)).To(Succeed())
	t.Cleanup(func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		err := cli.Delete(cleanupCtx, claim, client.Preconditions{UID: &claim.UID})
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			t.Errorf("cleaning baseline claim: %v", err)
		}
	})

	g.Eventually(func(g Gomega) {
		current := &infraapi.DatabaseClaim{}
		g.Expect(cli.Get(ctx, client.ObjectKeyFromObject(claim), current)).To(Succeed())
		condition := conditions.FindStatusCondition(current, "Provisioned")
		g.Expect(condition).NotTo(BeNil())
		g.Expect(condition.Status).To(Equal(metav1.ConditionTrue),
			"claim must provision against PostgreSQL under the baseline: %s", condition.Message)
	}).WithTimeout(3*time.Minute).Should(Succeed(), "claim did not provision under the baseline")

	credentials := &corev1.Secret{}
	g.Eventually(func(g Gomega) {
		g.Expect(cli.Get(ctx, types.NamespacedName{
			Namespace: consumerNamespace.Name, Name: claim.Name,
		}, credentials)).To(Succeed())
	}).WithTimeout(time.Minute).Should(Succeed())

	// The consumer asserts INSERT/SELECT on a contract table the claim role
	// is granted; an admin connection creates it (the claim role cannot
	// CREATE in schema public on PostgreSQL 15+), mirroring
	// TestInternalProviderClaimLifecycle.
	internalAdminSecret := &corev1.Secret{}
	g.Expect(cli.Get(ctx, types.NamespacedName{
		Namespace: namespace,
		Name:      dbcontroller.InternalAdminSecretName(provider.Name),
	}, internalAdminSecret)).To(Succeed())
	adminSecretCopy := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "claim-contract-admin-", Namespace: consumerNamespace.Name},
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
	createAdminContractTable(t, ctx, cli, restCfg, consumerNamespace.Name,
		adminSecretCopy.Name, string(credentials.Data["dbname"]),
		string(credentials.Data["user"]), "disable", image, false)
	runConsumer(t, ctx, cli, restCfg, consumerNamespace.Name, claim.Name, image, false)

	// Remove the baseline before the t.Cleanup stack runs, so claim and
	// provider teardown (finalizer reconciliation) happens with normal
	// networking restored. The registered cleanup is the failure-path
	// fallback only.
	removeBaseline()
}

// networkPolicyEnforced reports whether this cluster's CNI enforces
// NetworkPolicy, by attempting a real connection under a deny-all policy:
// first a positive control proving the probe pods can connect at all, then
// the same connection under deny-all, which must be blocked. A cluster whose
// CNI ignores policies (kind's default kindnet) returns false, so callers can
// skip enforcement assertions instead of passing them vacuously.
func networkPolicyEnforced(
	t *testing.T,
	ctx context.Context,
	cli client.Client,
	image string,
) bool {
	t.Helper()
	g := NewWithT(t)

	probeNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "odh-db-npenforce-"}}
	g.Expect(cli.Create(ctx, probeNamespace)).To(Succeed())
	t.Cleanup(func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		if err := support.DeleteNamespace(cleanupCtx, cli, probeNamespace.Name, probeNamespace.UID); err != nil {
			t.Errorf("cleaning NetworkPolicy enforcement probe namespace: %v", err)
		}
	})

	// The listener: a stock PostgreSQL server, using the same image the
	// consumer assertions use, so no extra image is pulled.
	server := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "np-probe-server", Namespace: probeNamespace.Name},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:  "server",
				Image: image,
				Env:   []corev1.EnvVar{{Name: "POSTGRES_PASSWORD", Value: "probe"}},
			}},
		},
	}
	g.Expect(cli.Create(ctx, server)).To(Succeed())

	serverIP := ""
	g.Eventually(func(g Gomega) {
		current := &corev1.Pod{}
		g.Expect(cli.Get(ctx, client.ObjectKeyFromObject(server), current)).To(Succeed())
		g.Expect(current.Status.Phase).To(Equal(corev1.PodRunning),
			"NetworkPolicy probe server did not start: %v", current.Status.ContainerStatuses)
		g.Expect(current.Status.PodIP).NotTo(BeEmpty())
		serverIP = current.Status.PodIP
	}).WithTimeout(3*time.Minute).Should(Succeed(), "NetworkPolicy probe server never became Running")

	// connect runs a client pod that attempts a TCP connection to the probe
	// server, retrying up to attempts times, and reports whether any attempt
	// connected.
	connect := func(name string, attempts int) bool {
		connected := false
		clientPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: probeNamespace.Name},
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers: []corev1.Container{{
					Name:    "client",
					Image:   image,
					Command: []string{"/bin/bash", "-ec"},
					Args: []string{fmt.Sprintf(`
for i in $(seq 1 %d); do
  if timeout 5 bash -c "exec 3<>/dev/tcp/${SERVER_IP}/5432"; then
    exit 0
  fi
  sleep 1
done
exit 1
`, attempts)},
					Env: []corev1.EnvVar{{Name: "SERVER_IP", Value: serverIP}},
				}},
			},
		}
		g.Expect(cli.Create(ctx, clientPod)).To(Succeed())
		g.Eventually(func(g Gomega) {
			current := &corev1.Pod{}
			g.Expect(cli.Get(ctx, client.ObjectKeyFromObject(clientPod), current)).To(Succeed())
			switch current.Status.Phase {
			case corev1.PodSucceeded:
				connected = true
			case corev1.PodFailed:
				connected = false
			default:
				g.Expect(current.Status.Phase).To(BeElementOf(corev1.PodSucceeded, corev1.PodFailed),
					"probe client still %s", current.Status.Phase)
			}
		}).WithTimeout(time.Duration(attempts)*time.Second + time.Minute).Should(Succeed())
		return connected
	}

	// Positive control: without any policy, the connection must succeed.
	// A failure here is a test-setup error, not evidence about enforcement.
	g.Expect(connect("np-probe-control", 30)).To(BeTrue(),
		"NetworkPolicy probe positive control failed: the probe server was never reachable without any policy")

	denyAll := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "np-probe-deny-all", Namespace: probeNamespace.Name},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
	g.Expect(cli.Create(ctx, denyAll)).To(Succeed())

	// Under deny-all the same connection must now fail. CNI policy
	// programming is asynchronous, so a single succeeding attempt is not
	// evidence of non-enforcement -- retry for a window and treat the FIRST
	// blocked connection as proof of enforcement. Only sustained success
	// means the CNI ignores policies.
	// Each attempt needs its own pod: completed pods are not removed, so
	// reusing one name makes the next Create fail with AlreadyExists --
	// exactly the converging-policy race this window exists to absorb.
	attempt := 0
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		attempt++
		if !connect(fmt.Sprintf("np-probe-deny-%d", attempt), 1) {
			return true
		}
		// Connected: either the policy is not yet programmed or the CNI
		// does not enforce. Keep trying until the window closes.
		time.Sleep(2 * time.Second)
	}
	return false
}

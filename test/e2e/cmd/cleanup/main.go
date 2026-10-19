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

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	servicesv1alpha1 "github.com/opendatahub-io/opendatahub-db-operator/api/services/v1alpha1"
	modulemanager "github.com/opendatahub-io/opendatahub-db-operator/pkg/manager"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	e2eOwnerLabel  = "shiftleft.opendatahub.io/owner"
	e2eOwnerValue  = "rhai-db-operator"
	helmOwnerLabel = "owner"
	helmOwnerValue = "helm"
	helmNameLabel  = "name"
)

var errTeardownNoop = errors.New("no shift-left installation is present")

func main() {
	err := run(os.Args[1:])
	if errors.Is(err, errTeardownNoop) {
		os.Exit(3)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("expected a cleanup mode")
	}

	switch args[0] {
	case "database-service":
		return cleanupDatabaseService()
	case "prepare-namespace":
		name, err := namespaceName(args[1:])
		if err != nil {
			return err
		}
		return prepareNamespace(name)
	case "namespace":
		name, err := namespaceName(args[1:])
		if err != nil {
			return err
		}
		return cleanupNamespace(name)
	case "preflight":
		namespace, release, err := installationNames(args[1:])
		if err != nil {
			return err
		}
		return preflight(namespace, release)
	case "check-prerequisites":
		name, err := namespaceName(args[1:])
		if err != nil {
			return err
		}
		return checkPrerequisites(name)
	case "authorize-teardown":
		namespace, release, err := installationNames(args[1:])
		if err != nil {
			return err
		}
		authorized, err := authorizeTeardown(namespace, release)
		if err != nil {
			return err
		}
		if !authorized {
			return errTeardownNoop
		}
		return nil
	case "diagnostics":
		name, err := namespaceName(args[1:])
		if err != nil {
			return err
		}
		return diagnostics(name)
	default:
		return fmt.Errorf("unknown cleanup mode %q", args[0])
	}
}

func preflight(namespace, release string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	kubeClient, err := newClient()
	if err != nil {
		return err
	}
	return preflightWithClient(ctx, kubeClient, namespace, release)
}

func preflightWithClient(ctx context.Context, kubeClient client.Client, namespace, release string) error {
	namespaceObject := &corev1.Namespace{}
	namespaceExists := true
	err := kubeClient.Get(ctx, client.ObjectKey{Name: namespace}, namespaceObject)
	switch {
	case apierrors.IsNotFound(err):
		namespaceExists = false
	case err != nil:
		return fmt.Errorf("checking e2e namespace %q: %w", namespace, err)
	case namespaceObject.Labels[e2eOwnerLabel] != e2eOwnerValue:
		return fmt.Errorf("refusing to use namespace %q because it is not owned by the shift-left test", namespace)
	case namespaceObject.DeletionTimestamp != nil:
		return fmt.Errorf("refusing to use namespace %q because it is being deleted", namespace)
	}

	if namespaceExists {
		if err := verifyHelmReleaseOwnership(ctx, kubeClient, namespace, release); err != nil {
			return err
		}
	}
	singleton := &servicesv1alpha1.DatabaseService{}
	err = kubeClient.Get(ctx, client.ObjectKey{Name: servicesv1alpha1.DatabaseServiceInstanceName}, singleton)
	if isGone(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking DatabaseService singleton before installation: %w", err)
	}
	return fmt.Errorf("refusing to install while DatabaseService singleton %q already exists", singleton.Name)
}

func checkPrerequisites(namespace string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	kubeClient, err := newClient()
	if err != nil {
		return err
	}
	return checkPrerequisitesWithClient(ctx, kubeClient, namespace)
}

func checkPrerequisitesWithClient(ctx context.Context, kubeClient client.Client, namespace string) error {
	namespaceObject := &corev1.Namespace{}
	if err := kubeClient.Get(ctx, client.ObjectKey{Name: namespace}, namespaceObject); err != nil {
		return fmt.Errorf("checking e2e namespace %q before prerequisite checks: %w", namespace, err)
	}
	if namespaceObject.Labels[e2eOwnerLabel] != e2eOwnerValue {
		return fmt.Errorf("refusing prerequisite checks in namespace %q because it is not owned by the shift-left test", namespace)
	}
	if namespaceObject.DeletionTimestamp != nil {
		return fmt.Errorf("refusing prerequisite checks in namespace %q because it is being deleted", namespace)
	}
	for _, kind := range []string{"Certificate", "Issuer"} {
		if err := checkCertManagerAPI(ctx, kubeClient, namespace, kind); err != nil {
			return err
		}
	}
	return nil
}

func authorizeTeardown(namespace, release string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	kubeClient, err := newClient()
	if err != nil {
		return false, err
	}
	return authorizeTeardownWithClient(ctx, kubeClient, namespace, release)
}

func authorizeTeardownWithClient(
	ctx context.Context,
	kubeClient client.Client,
	namespace, release string,
) (bool, error) {
	namespaceObject := &corev1.Namespace{}
	namespaceErr := kubeClient.Get(ctx, client.ObjectKey{Name: namespace}, namespaceObject)
	namespaceExists := namespaceErr == nil
	if namespaceErr != nil && !apierrors.IsNotFound(namespaceErr) {
		return false, fmt.Errorf("checking e2e namespace %q before teardown: %w", namespace, namespaceErr)
	}
	if namespaceExists {
		if namespaceObject.Labels[e2eOwnerLabel] != e2eOwnerValue {
			return false, fmt.Errorf("refusing to tear down namespace %q because it is not owned by the shift-left test", namespace)
		}
		if namespaceObject.DeletionTimestamp != nil {
			return false, fmt.Errorf("refusing to tear down namespace %q because it is already being deleted", namespace)
		}
	}

	singleton := &servicesv1alpha1.DatabaseService{}
	singletonErr := kubeClient.Get(
		ctx,
		client.ObjectKey{Name: servicesv1alpha1.DatabaseServiceInstanceName},
		singleton,
	)
	singletonExists := singletonErr == nil
	if singletonErr != nil && !isGone(singletonErr) {
		return false, fmt.Errorf("checking DatabaseService singleton before teardown: %w", singletonErr)
	}
	if singletonExists && singleton.Labels[e2eOwnerLabel] != e2eOwnerValue {
		return false, fmt.Errorf("refusing to tear down because DatabaseService singleton %q is not owned by the shift-left test", singleton.Name)
	}
	if !namespaceExists {
		if singletonExists {
			return false, fmt.Errorf("owned DatabaseService singleton %q remains without namespace %q; recover it explicitly", singleton.Name, namespace)
		}
		return false, nil
	}

	if err := verifyHelmReleaseOwnership(ctx, kubeClient, namespace, release); err != nil {
		return false, err
	}
	return true, nil
}

func verifyHelmReleaseOwnership(ctx context.Context, kubeClient client.Client, namespace, release string) error {
	secrets := &corev1.SecretList{}
	if err := kubeClient.List(
		ctx,
		secrets,
		client.InNamespace(namespace),
		client.MatchingLabels{helmOwnerLabel: helmOwnerValue, helmNameLabel: release},
	); err != nil {
		return fmt.Errorf("checking Helm release metadata for %q in namespace %q: %w", release, namespace, err)
	}
	for _, secret := range secrets.Items {
		if secret.Labels[e2eOwnerLabel] != e2eOwnerValue {
			return fmt.Errorf("refusing to use Helm release %q because revision %q is not owned by the shift-left test", release, secret.Name)
		}
	}
	return nil
}

func checkCertManagerAPI(ctx context.Context, kubeClient client.Client, namespace, kind string) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "cert-manager.io", Version: "v1", Kind: kind + "List",
	})
	if err := kubeClient.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("checking required cert-manager.io/v1 %s API: %w", kind, err)
	}
	return nil
}

func cleanupDatabaseService() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	kubeClient, err := newClient()
	if err != nil {
		return err
	}

	return cleanupDatabaseServiceWithClient(ctx, kubeClient)
}

func cleanupDatabaseServiceWithClient(ctx context.Context, kubeClient client.Client) error {
	key := client.ObjectKey{Name: servicesv1alpha1.DatabaseServiceInstanceName}
	instance := &servicesv1alpha1.DatabaseService{}
	if err := kubeClient.Get(ctx, key, instance); isGone(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("getting DatabaseService singleton: %w", err)
	}
	if instance.Labels[e2eOwnerLabel] != e2eOwnerValue {
		fmt.Fprintln(os.Stderr, "leaving DatabaseService singleton because it is not owned by the shift-left test")
		return nil
	}

	uid := instance.UID
	if err := kubeClient.Delete(ctx, instance, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting DatabaseService singleton: %w", err)
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		current := &servicesv1alpha1.DatabaseService{}
		err := kubeClient.Get(ctx, key, current)
		if isGone(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("waiting for DatabaseService singleton deletion: %w", err)
		}
		if current.UID != uid {
			return fmt.Errorf("DatabaseService singleton was replaced during cleanup; refusing to delete the replacement")
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for DatabaseService singleton deletion: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func prepareNamespace(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	kubeClient, err := newClient()
	if err != nil {
		return err
	}

	namespace := &corev1.Namespace{}
	err = kubeClient.Get(ctx, client.ObjectKey{Name: name}, namespace)
	if apierrors.IsNotFound(err) {
		namespace = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					e2eOwnerLabel: e2eOwnerValue,
				},
			},
		}
		if err := kubeClient.Create(ctx, namespace); err != nil {
			return fmt.Errorf("creating e2e namespace %q: %w", name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting namespace %q: %w", name, err)
	}
	if namespace.Labels[e2eOwnerLabel] != e2eOwnerValue {
		return fmt.Errorf("refusing to use existing namespace %q because it is not owned by the shift-left test", name)
	}
	if namespace.DeletionTimestamp != nil {
		return fmt.Errorf("namespace %q is already being deleted", name)
	}
	return nil
}

func cleanupNamespace(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	kubeClient, err := newClient()
	if err != nil {
		return err
	}

	return cleanupNamespaceWithClient(ctx, kubeClient, name)
}

func cleanupNamespaceWithClient(ctx context.Context, kubeClient client.Client, name string) error {
	namespace := &corev1.Namespace{}
	if err := kubeClient.Get(ctx, client.ObjectKey{Name: name}, namespace); isGone(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("getting namespace %q: %w", name, err)
	}
	if namespace.Labels[e2eOwnerLabel] != e2eOwnerValue {
		return fmt.Errorf("refusing to delete namespace %q because it is not owned by the shift-left test", name)
	}

	uid := namespace.UID
	if err := kubeClient.Delete(
		ctx,
		namespace,
		client.Preconditions{UID: &uid},
	); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting namespace %q: %w", name, err)
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		current := &corev1.Namespace{}
		err := kubeClient.Get(ctx, client.ObjectKey{Name: name}, current)
		if isGone(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("waiting for namespace %q deletion: %w", name, err)
		}
		if current.UID != uid {
			return fmt.Errorf("namespace %q was replaced during cleanup; refusing to delete the replacement", name)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for namespace %q deletion: %w", name, ctx.Err())
		case <-ticker.C:
		}
	}
}

func namespaceName(args []string) (string, error) {
	flags := flag.NewFlagSet("namespace", flag.ContinueOnError)
	name := flags.String("name", "", "namespace name")
	if err := flags.Parse(args); err != nil {
		return "", err
	}
	if *name == "" {
		return "", fmt.Errorf("namespace operation requires --name")
	}
	return *name, nil
}

func installationNames(args []string) (string, string, error) {
	flags := flag.NewFlagSet("installation", flag.ContinueOnError)
	name := flags.String("name", "", "namespace name")
	release := flags.String("release", "", "Helm release name")
	if err := flags.Parse(args); err != nil {
		return "", "", err
	}
	if *name == "" || *release == "" {
		return "", "", fmt.Errorf("installation operation requires --name and --release")
	}
	return *name, *release, nil
}

func diagnostics(namespace string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	kubeClient, err := newClient()
	if err != nil {
		return err
	}
	return diagnosticsWithClient(ctx, kubeClient, namespace, os.Stdout)
}

func diagnosticsWithClient(ctx context.Context, kubeClient client.Client, namespace string, output io.Writer) error {
	namespaceObject := &corev1.Namespace{}
	if err := kubeClient.Get(ctx, client.ObjectKey{Name: namespace}, namespaceObject); err != nil {
		if apierrors.IsNotFound(err) {
			_, writeErr := fmt.Fprintf(output, "namespace=%s phase=Missing\n", namespace)
			return writeErr
		}
		return fmt.Errorf("reading namespace diagnostics: %w", err)
	}
	if _, err := fmt.Fprintf(output, "namespace=%s phase=%s terminating=%t\n", namespace,
		namespaceObject.Status.Phase, namespaceObject.DeletionTimestamp != nil); err != nil {
		return err
	}

	deployments := &appsv1.DeploymentList{}
	if err := kubeClient.List(ctx, deployments, client.InNamespace(namespace), client.Limit(20)); err != nil {
		return fmt.Errorf("listing deployment diagnostics: %w", err)
	}
	for _, deployment := range deployments.Items {
		desired := int32(1)
		if deployment.Spec.Replicas != nil {
			desired = *deployment.Spec.Replicas
		}
		if _, err := fmt.Fprintf(output, "deployment=%s desired=%d ready=%d available=%d\n",
			deployment.Name, desired, deployment.Status.ReadyReplicas, deployment.Status.AvailableReplicas); err != nil {
			return err
		}
	}

	pods := &corev1.PodList{}
	if err := kubeClient.List(ctx, pods, client.InNamespace(namespace), client.Limit(20)); err != nil {
		return fmt.Errorf("listing pod diagnostics: %w", err)
	}
	for _, pod := range pods.Items {
		if _, err := fmt.Fprintf(output, "pod=%s phase=%s\n", pod.Name, pod.Status.Phase); err != nil {
			return err
		}
		for _, status := range pod.Status.ContainerStatuses {
			state := "running"
			if status.State.Waiting != nil {
				state = "waiting:" + status.State.Waiting.Reason
			} else if status.State.Terminated != nil {
				state = fmt.Sprintf("terminated:%d", status.State.Terminated.ExitCode)
			}
			if _, err := fmt.Fprintf(output, "pod=%s container=%s state=%s\n", pod.Name, status.Name, state); err != nil {
				return err
			}
		}
	}

	events := &corev1.EventList{}
	if err := kubeClient.List(ctx, events, client.InNamespace(namespace), client.Limit(50)); err != nil {
		return fmt.Errorf("listing event diagnostics: %w", err)
	}
	sort.Slice(events.Items, func(i, j int) bool {
		return eventTime(events.Items[i]).Before(eventTime(events.Items[j]))
	})
	if len(events.Items) > 20 {
		events.Items = events.Items[len(events.Items)-20:]
	}
	for _, event := range events.Items {
		if _, err := fmt.Fprintf(output, "event=%s type=%s reason=%s object=%s/%s\n",
			event.Name, event.Type, event.Reason, event.InvolvedObject.Kind, event.InvolvedObject.Name); err != nil {
			return err
		}
	}
	return nil
}

func eventTime(event corev1.Event) time.Time {
	if !event.EventTime.IsZero() {
		return event.EventTime.Time
	}
	if !event.LastTimestamp.IsZero() {
		return event.LastTimestamp.Time
	}
	return event.CreationTimestamp.Time
}

func newClient() (client.Client, error) {
	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("loading cluster config: %w", err)
	}

	scheme, err := modulemanager.NewScheme()
	if err != nil {
		return nil, fmt.Errorf("creating Kubernetes scheme: %w", err)
	}

	kubeClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("creating Kubernetes client: %w", err)
	}
	return kubeClient, nil
}

func isGone(err error) bool {
	return apierrors.IsNotFound(err) || meta.IsNoMatchError(err)
}

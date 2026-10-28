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

// Package support holds shared helpers for cluster-backed tests.
package support

import (
	"context"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DefaultEventuallyTimeout bounds how long Gomega's Eventually() waits
	// by default across this module's integration/e2e suites.
	DefaultEventuallyTimeout = 90 * time.Second
	// DefaultEventuallyPollingInterval is Gomega's default Eventually() poll
	// interval.
	DefaultEventuallyPollingInterval = 2 * time.Second
	// DefaultConsistentlyPollingInterval is Gomega's default Consistently()
	// poll interval.
	DefaultConsistentlyPollingInterval = 2 * time.Second

	// namespaceDeletionPollInterval is how often WaitNamespaceDeletion checks
	// whether the namespace has finished terminating.
	namespaceDeletionPollInterval = 2 * time.Second

	// DefaultIntegrationTestNamespace is the namespace integration tests
	// operate in when INTEGRATION_TEST_NAMESPACE isn't set.
	DefaultIntegrationTestNamespace = "opendatahub-db-operator-integration"
)

// GomegaConfig holds the Gomega timing defaults for this module's test
// suites, overridable via environment variables so CI and local runs can
// tune them independently of the source.
type GomegaConfig struct {
	EventuallyTimeout           time.Duration
	EventuallyPollingInterval   time.Duration
	ConsistentlyPollingInterval time.Duration
}

// LoadGomegaConfig reads Gomega timing overrides from the environment,
// falling back to this module's compiled defaults.
func LoadGomegaConfig() GomegaConfig {
	const (
		eventuallyTimeoutEnv   = "ODH_MODULE_OPERATOR_TEST_EVENTUALLY_TIMEOUT"
		eventuallyPollingEnv   = "ODH_MODULE_OPERATOR_TEST_EVENTUALLY_POLLING_INTERVAL"
		consistentlyPollingEnv = "ODH_MODULE_OPERATOR_TEST_CONSISTENTLY_POLLING_INTERVAL"
	)

	return GomegaConfig{
		EventuallyTimeout:           durationEnv(eventuallyTimeoutEnv, DefaultEventuallyTimeout),
		EventuallyPollingInterval:   durationEnv(eventuallyPollingEnv, DefaultEventuallyPollingInterval),
		ConsistentlyPollingInterval: durationEnv(consistentlyPollingEnv, DefaultConsistentlyPollingInterval),
	}
}

func durationEnv(key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}

	d, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}

	return d
}

// IntegrationTestNamespace returns the namespace integration tests should
// use, honoring INTEGRATION_TEST_NAMESPACE if set.
func IntegrationTestNamespace() string {
	if ns := os.Getenv("INTEGRATION_TEST_NAMESPACE"); ns != "" {
		return ns
	}

	return DefaultIntegrationTestNamespace
}

// EnsureNamespace creates the given namespace if it doesn't already exist.
// Leader election creates a Lease in this namespace, so create it before starting the manager.
//
// Returns the namespace's UID and whether this call is the one that created
// it. Callers that intend to delete the namespace afterward must check
// created and pass the returned UID to DeleteNamespace's precondition --
// otherwise a namespace that already existed for an unrelated reason (a
// name collision with another concurrent run, or real cluster state) would
// be deleted out from under whatever owns it.
func EnsureNamespace(ctx context.Context, cli client.Client, name string) (types.UID, bool, error) {
	if cli == nil {
		return "", false, fmt.Errorf("client is nil")
	}

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}

	if err := cli.Create(ctx, ns); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", false, fmt.Errorf("creating namespace %q: %w", name, err)
		}

		existing := &corev1.Namespace{}
		if err := cli.Get(ctx, client.ObjectKeyFromObject(ns), existing); err != nil {
			return "", false, fmt.Errorf("reading pre-existing namespace %q: %w", name, err)
		}

		return existing.UID, false, nil
	}

	return ns.UID, true, nil
}

// DeleteNamespace removes the given namespace, but only if its current UID
// still matches expectedUID -- a precondition that ensures this only ever
// deletes the exact namespace object the caller created (or was told about),
// never a same-named namespace that was deleted and recreated by something
// else in the meantime. Not-found and precondition-conflict are both treated
// as a clean outcome: in both cases, the namespace this call owned is gone
// (or was never there to begin with).
func DeleteNamespace(ctx context.Context, cli client.Client, name string, expectedUID types.UID) error {
	if cli == nil {
		return fmt.Errorf("client is nil")
	}

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}

	err := cli.Delete(ctx, ns, client.Preconditions{UID: &expectedUID})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return fmt.Errorf("deleting namespace %q: %w", name, err)
	}

	return nil
}

// WaitNamespaceDeletion blocks until the namespace is gone. A same-named
// namespace with a different UID counts as gone: it is not the object the
// caller deleted. The context bounds the wait; expiry returns an error so
// teardown fails loudly instead of deferring the problem to the next run.
func WaitNamespaceDeletion(ctx context.Context, cli client.Client, name string, deletedUID types.UID) error {
	if cli == nil {
		return fmt.Errorf("client is nil")
	}

	ticker := time.NewTicker(namespaceDeletionPollInterval)
	defer ticker.Stop()

	for {
		existing := &corev1.Namespace{}
		err := cli.Get(ctx, client.ObjectKey{Name: name}, existing)
		switch {
		case apierrors.IsNotFound(err):
			return nil
		case err != nil:
			return fmt.Errorf("reading namespace %q: %w", name, err)
		case existing.UID != deletedUID:
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("namespace %q still terminating: %w", name, ctx.Err())
		case <-ticker.C:
		}
	}
}

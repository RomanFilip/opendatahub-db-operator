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

package manager

import (
	"context"
	"fmt"
	"sync"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infraApi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
	"github.com/opendatahub-io/opendatahub-db-operator/internal/controller/databaseclaim"
	"github.com/opendatahub-io/opendatahub-db-operator/internal/controller/databaseprovider"
	"github.com/opendatahub-io/opendatahub-db-operator/internal/controller/schemaclaim"
	moduleconfig "github.com/opendatahub-io/opendatahub-db-operator/pkg/config"
)

// NewInfrastructureRegistration returns a function that registers the
// infrastructure controllers (DatabaseProvider, DatabaseClaim, SchemaClaim)
// on mgr. New invokes it exactly once, at startup, and the once outcome is
// final: when CRDs are absent at that call, registration is skipped and
// nil is returned — the pre-existing behavior where installing CRDs later
// requires a pod restart. A later call therefore cannot register after
// installation; restart-free registration needs "CRDs absent" treated as
// retryable rather than a completed no-op, which the follow-up for
// RHOAIENG-100934 replaces these semantics to deliver. A failed attempt is
// likewise sticky — the error is returned on every later call instead of
// being retried — because registration failures are setup errors such as
// scheme or name conflicts that a retry cannot fix. The guard is per
// returned function, not per manager.
func NewInfrastructureRegistration(
	ctx context.Context,
	mgr ctrl.Manager,
	cfg *moduleconfig.Config,
) func() error {
	return onceGuarded(func() error {
		return registerInfrastructureControllers(ctx, mgr, cfg)
	})
}

// onceGuarded wraps fn so it runs at most once; the outcome of that single
// attempt is returned on every call.
func onceGuarded(fn func() error) func() error {
	var once sync.Once
	var err error
	return func() error {
		once.Do(func() {
			err = fn()
		})
		return err
	}
}

// registerInfrastructureControllers registers the DatabaseProvider,
// DatabaseClaim, and SchemaClaim reconcilers on mgr when their CRDs are
// present, so the operator can still reconcile the module singleton when
// infrastructure CRDs are not installed yet.
func registerInfrastructureControllers(
	ctx context.Context,
	mgr ctrl.Manager,
	cfg *moduleconfig.Config,
) error {
	reader := mgr.GetAPIReader()
	log := ctrl.Log.WithName("manager")

	providerCRDAvailable, err := crdAvailable(ctx, reader, infraApi.DatabaseProviderCRDName)
	if err != nil {
		return fmt.Errorf("checking DatabaseProvider CRD: %w", err)
	}
	if providerCRDAvailable {
		if err := databaseprovider.NewReconciler(ctx, mgr, cfg, databaseprovider.Options{
			Recorder: mgr.GetEventRecorder(infraApi.DatabaseProviderResource),
		}); err != nil {
			return fmt.Errorf("creating databaseprovider reconciler: %w", err)
		}
	} else {
		log.Info("DatabaseProvider CRD is absent; provider reconciliation requires a restart after CRD installation")
	}

	// Claims watch DatabaseProvider, so both the claim CRD and provider CRD
	// must be present for each claim reconciler.
	databaseClaimCRDAvailable, err := crdAvailable(ctx, reader, infraApi.DatabaseClaimCRDName)
	if err != nil {
		return fmt.Errorf("checking DatabaseClaim CRD: %w", err)
	}
	if databaseClaimCRDAvailable && providerCRDAvailable {
		if err := databaseclaim.NewReconciler(ctx, mgr, cfg, databaseclaim.Options{
			Recorder: mgr.GetEventRecorder(infraApi.DatabaseClaimResource),
		}); err != nil {
			return fmt.Errorf("creating databaseclaim reconciler: %w", err)
		}
	} else {
		log.Info("DatabaseClaim reconciliation skipped; requires claim and provider CRDs (restart after install)")
	}

	schemaClaimCRDAvailable, err := crdAvailable(ctx, reader, infraApi.SchemaClaimCRDName)
	if err != nil {
		return fmt.Errorf("checking SchemaClaim CRD: %w", err)
	}
	if schemaClaimCRDAvailable && providerCRDAvailable {
		if err := schemaclaim.NewReconciler(ctx, mgr, cfg, schemaclaim.Options{
			Recorder: mgr.GetEventRecorder(infraApi.SchemaClaimResource),
		}); err != nil {
			return fmt.Errorf("creating schemaclaim reconciler: %w", err)
		}
	} else {
		log.Info("SchemaClaim reconciliation skipped; requires claim and provider CRDs (restart after install)")
	}

	return nil
}

// crdAvailable reports whether the named CRD exists.
func crdAvailable(ctx context.Context, reader client.Reader, name string) (bool, error) {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	err := reader.Get(ctx, client.ObjectKey{Name: name}, crd)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

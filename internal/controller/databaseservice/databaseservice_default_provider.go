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

package databaseservice

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	odherrors "github.com/opendatahub-io/odh-platform-utilities/framework/controller/actions/errors"
	"github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
	odhtypes "github.com/opendatahub-io/odh-platform-utilities/framework/controller/types"
	infraApi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
	servicesv1alpha1 "github.com/opendatahub-io/opendatahub-db-operator/api/services/v1alpha1"
	moduleconfig "github.com/opendatahub-io/opendatahub-db-operator/pkg/config"
	dbcontroller "github.com/opendatahub-io/opendatahub-db-operator/pkg/controller"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	conditionDefaultProviderReady   = "DefaultProviderReady"
	defaultProviderCleanupFinalizer = "services.platform.opendatahub.io/default-provider-cleanup"
	providerRetentionFinalizer      = "services.platform.opendatahub.io/retain-default-provider-data"
	defaultProviderOwnerAnnotation  = dbcontroller.AnnotationDefaultProviderOwner
)

func (m *Module) defaultProviderName() string {
	if m.cfg.DefaultProviderName != "" {
		return m.cfg.DefaultProviderName
	}
	return moduleconfig.DefaultProviderName
}

func (m *Module) ensureDefaultProvider(ctx context.Context, rr *odhtypes.ReconciliationRequest) error {
	service, ok := rr.Instance.(*servicesv1alpha1.DatabaseService)
	if !ok {
		return fmt.Errorf("instance is not a DatabaseService")
	}
	name := m.defaultProviderName()
	if service.Status.DefaultProviderName == "" {
		service.Status.DefaultProviderName = name
		rr.Conditions.MarkFalse(conditionDefaultProviderReady,
			conditions.WithReason("Initializing"),
			conditions.WithMessagef("recorded default provider name %q", name))
		return odherrors.NewStopErrorW(fmt.Errorf("recording default provider name")).WithRequeueAfter(time.Second)
	}
	if service.Status.DefaultProviderName != name {
		message := fmt.Sprintf("default provider name is immutable: installed as %q, configured as %q", service.Status.DefaultProviderName, name)
		rr.Conditions.MarkFalse(conditionDefaultProviderReady,
			conditions.WithReason("NameChanged"), conditions.WithMessage(message))
		return odherrors.NewStopErrorW(fmt.Errorf("%s", message)).WithRequeueAfter(retryInterval(m.cfg))
	}

	provider := &infraApi.DatabaseProvider{}
	err := rr.Client.Get(ctx, client.ObjectKey{Name: name}, provider)
	if meta.IsNoMatchError(err) {
		message := "DatabaseProvider API is not installed"
		rr.Conditions.MarkFalse(conditionDefaultProviderReady,
			conditions.WithReason("ProviderAPIUnavailable"), conditions.WithMessage(message))
		return odherrors.NewStopErrorW(fmt.Errorf("%s", message)).WithRequeueAfter(retryInterval(m.cfg))
	}
	if apierrors.IsNotFound(err) {
		provider = &infraApi.DatabaseProvider{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Annotations: map[string]string{defaultProviderOwnerAnnotation: service.Name},
			},
			Spec: infraApi.DatabaseProviderSpec{
				Type: infraApi.ProviderTypeInternal,
				Internal: &infraApi.InternalProviderSpec{
					Storage: infraApi.StorageSpec{Size: resource.MustParse("20Gi")},
				},
			},
		}
		if err := rr.Client.Create(ctx, provider); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating default DatabaseProvider %q: %w", name, err)
		}
		rr.Conditions.MarkFalse(conditionDefaultProviderReady,
			conditions.WithReason("Provisioning"),
			conditions.WithMessagef("default DatabaseProvider %q is provisioning", name))
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting default DatabaseProvider %q: %w", name, err)
	}
	if provider.Annotations[defaultProviderOwnerAnnotation] != service.Name || provider.Spec.Type != infraApi.ProviderTypeInternal {
		message := fmt.Sprintf("DatabaseProvider %q already exists and is not this service's Internal default provider", name)
		rr.Conditions.MarkFalse(conditionDefaultProviderReady,
			conditions.WithReason("NameConflict"), conditions.WithMessage(message))
		return odherrors.NewStopErrorW(fmt.Errorf("%s", message)).WithRequeueAfter(retryInterval(m.cfg))
	}
	if dbcontroller.ProviderReachable(provider.Status.Conditions) {
		rr.Conditions.MarkTrue(conditionDefaultProviderReady,
			conditions.WithReason("ProviderReady"),
			conditions.WithMessagef("default DatabaseProvider %q is reachable", name))
	} else {
		rr.Conditions.MarkFalse(conditionDefaultProviderReady,
			conditions.WithReason("Provisioning"),
			conditions.WithMessagef("default DatabaseProvider %q is not yet reachable", name))
	}
	return nil
}

func (m *Module) cleanupDefaultProvider(ctx context.Context, rr *odhtypes.ReconciliationRequest) error {
	service, ok := rr.Instance.(*servicesv1alpha1.DatabaseService)
	if !ok {
		return fmt.Errorf("instance is not a DatabaseService")
	}
	name := service.Status.DefaultProviderName
	if name == "" {
		name = m.defaultProviderName()
	}
	reader := m.apiReader
	if reader == nil {
		reader = rr.Client
	}
	provider := &infraApi.DatabaseProvider{}
	providerErr := reader.Get(ctx, client.ObjectKey{Name: name}, provider)
	if providerErr != nil {
		if !apierrors.IsNotFound(providerErr) && !meta.IsNoMatchError(providerErr) {
			return fmt.Errorf("getting default provider for cleanup: %w", providerErr)
		}
		provider = nil
	}
	if provider == nil || provider.Annotations[defaultProviderOwnerAnnotation] != service.Name {
		return nil
	}
	if provider.Spec.Type != infraApi.ProviderTypeInternal {
		return fmt.Errorf("default DatabaseProvider %q is owned by this DatabaseService but is not Internal", name)
	}
	claims, err := boundClaims(ctx, reader, name, provider)
	if err != nil {
		return err
	}
	if len(claims) != 0 {
		message := fmt.Sprintf("default provider %q is still used by claims: %s", name, strings.Join(claims, ", "))
		conditions.SetStatusCondition(service, fwapi.Condition{
			Type: conditionDefaultProviderReady, Status: metav1.ConditionFalse,
			Reason: "BlockingClaims", Message: message,
		})
		if err := rr.Client.Status().Update(ctx, service); err != nil {
			return fmt.Errorf("reporting claims blocking DatabaseService deletion: %w", err)
		}
		return fmt.Errorf("%s", message)
	}

	if provider.DeletionTimestamp == nil {
		if !slices.Contains(provider.Finalizers, providerRetentionFinalizer) {
			provider.Finalizers = append(provider.Finalizers, providerRetentionFinalizer)
			if err := rr.Client.Update(ctx, provider); err != nil {
				return fmt.Errorf("protecting default provider data during cleanup: %w", err)
			}
		}
		if err := rr.Client.Delete(ctx, provider); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting default DatabaseProvider: %w", err)
		}
		return fmt.Errorf("waiting for default provider deletion to begin")
	}

	namespace := dbcontroller.InternalNamespace(provider, m.cfg.OperatorNamespace)
	if err := detachProviderOwner(ctx, rr.Client, &corev1.PersistentVolumeClaim{}, client.ObjectKey{Namespace: namespace, Name: dbcontroller.InternalPVCName(name)}, provider.UID); err != nil {
		return fmt.Errorf("retaining default provider storage: %w", err)
	}
	if err := detachProviderOwner(ctx, rr.Client, &corev1.Secret{}, client.ObjectKey{Namespace: namespace, Name: dbcontroller.InternalAdminSecretName(name)}, provider.UID); err != nil {
		return fmt.Errorf("retaining default provider admin credentials: %w", err)
	}
	provider.Finalizers = slices.DeleteFunc(provider.Finalizers, func(f string) bool { return f == providerRetentionFinalizer })
	if err := rr.Client.Update(ctx, provider); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("finishing default provider cleanup: %w", err)
	}
	return fmt.Errorf("waiting for default DatabaseProvider %q to be removed", name)
}

func boundClaims(ctx context.Context, cli client.Reader, providerName string, provider *infraApi.DatabaseProvider) ([]string, error) {
	var names []string
	databases := &infraApi.DatabaseClaimList{}
	if err := cli.List(ctx, databases); err != nil && !meta.IsNoMatchError(err) {
		return nil, fmt.Errorf("listing DatabaseClaims before default provider deletion: %w", err)
	}
	for _, claim := range databases.Items {
		usesProvider, err := claimUsesProvider(claim.Spec.Provider, claim.Status.Provider, providerName, provider)
		if err != nil {
			return nil, fmt.Errorf("checking DatabaseClaim %s/%s provider selector: %w", claim.Namespace, claim.Name, err)
		}
		if usesProvider {
			names = append(names, fmt.Sprintf("DatabaseClaim %s/%s", claim.Namespace, claim.Name))
		}
	}
	schemas := &infraApi.SchemaClaimList{}
	if err := cli.List(ctx, schemas); err != nil && !meta.IsNoMatchError(err) {
		return nil, fmt.Errorf("listing SchemaClaims before default provider deletion: %w", err)
	}
	for _, claim := range schemas.Items {
		usesProvider, err := claimUsesProvider(claim.Spec.Provider, claim.Status.Provider, providerName, provider)
		if err != nil {
			return nil, fmt.Errorf("checking SchemaClaim %s/%s provider selector: %w", claim.Namespace, claim.Name, err)
		}
		if usesProvider {
			names = append(names, fmt.Sprintf("SchemaClaim %s/%s", claim.Namespace, claim.Name))
		}
	}
	slices.Sort(names)
	return names, nil
}

func claimUsesProvider(ref infraApi.ProviderRef, boundName, providerName string, provider *infraApi.DatabaseProvider) (bool, error) {
	if boundName == providerName || ref.Name == providerName {
		return true, nil
	}
	if boundName != "" || ref.Name != "" {
		return false, nil
	}
	if ref.Selector == nil {
		return true, nil
	}
	if provider == nil {
		return false, nil
	}
	selector, err := metav1.LabelSelectorAsSelector(ref.Selector)
	if err != nil {
		return false, err
	}
	return selector.Matches(labels.Set(provider.Labels)), nil
}

func detachProviderOwner(ctx context.Context, cli client.Client, obj client.Object, key client.ObjectKey, providerUID types.UID) error {
	if err := cli.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	before := obj.DeepCopyObject().(client.Object)
	refs := slices.DeleteFunc(obj.GetOwnerReferences(), func(ref metav1.OwnerReference) bool { return ref.UID == providerUID })
	if len(refs) == len(before.GetOwnerReferences()) {
		return nil
	}
	obj.SetOwnerReferences(refs)
	return cli.Patch(ctx, obj, client.MergeFrom(before))
}

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

package databaseprovider

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"

	"github.com/rs/xid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
	infraApi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
	moduleconfig "github.com/opendatahub-io/opendatahub-db-operator/pkg/config"
	dbcontroller "github.com/opendatahub-io/opendatahub-db-operator/pkg/controller"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/postgres"
	pginstance "github.com/opendatahub-io/opendatahub-db-operator/pkg/postgres/instance"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/resources/gvk"
	secretvalue "github.com/opendatahub-io/opendatahub-db-operator/pkg/secret"
)

var errInternalAdminCredentialsLost = errors.New("internal admin credentials lost")

var (
	defaultInternalPostgresRequests = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("500m"),
		corev1.ResourceMemory: resource.MustParse("1Gi"),
	}
	defaultInternalPostgresLimits = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("2"),
		corev1.ResourceMemory: resource.MustParse("4Gi"),
	}
)

type TLSState struct {
	Enabled       bool
	Ready         bool
	TLSSecret     *corev1.Secret
	TLSSecretHash string
	CAData        []byte
	AdminSecret   *corev1.Secret
}

func resolveInternalData(
	ctx context.Context,
	cli client.Client,
	provider *infraApi.DatabaseProvider,
	cfg *moduleconfig.Config,
	tlsState TLSState,
) (pginstance.Data, error) {
	if provider == nil {
		return pginstance.Data{}, fmt.Errorf("provider is nil")
	}
	if provider.Spec.Internal == nil {
		return pginstance.Data{}, fmt.Errorf("spec.internal is required for Internal providers")
	}

	namespaces, err := referencedClaimNamespaces(ctx, cli, provider)
	if err != nil {
		return pginstance.Data{}, fmt.Errorf("listing referenced claim namespaces: %w", err)
	}

	storageClassName := ""
	if provider.Spec.Internal.Storage.StorageClassName != nil {
		storageClassName = *provider.Spec.Internal.Storage.StorageClassName
	}

	data := pginstance.Data{
		Namespace:          dbcontroller.InternalNamespace(provider, cfg.OperatorNamespace),
		ProviderName:       provider.Name,
		ServiceAccountName: dbcontroller.InternalServiceAccountName(provider.Name),
		Service: pginstance.Service{
			Name: dbcontroller.InternalServiceName(provider.Name),
		},
		PVC: pginstance.PVC{
			Name:             dbcontroller.InternalPVCName(provider.Name),
			Size:             provider.Spec.Internal.Storage.Size.String(),
			StorageClassName: storageClassName,
		},
		InitDB: pginstance.InitDB{
			ConfigMapName: dbcontroller.InternalInitDBConfigMapName(provider.Name),
			Extensions:    append([]string(nil), provider.Spec.Internal.Extensions...),
		},
		Postgres: pginstance.Postgres{
			Image:           cfg.PostgresImage,
			Replicas:        pginstance.InternalPostgresReplicaCount,
			Resources:       internalPostgresResources(provider.Spec.Internal.Resources),
			AdminSecretName: dbcontroller.InternalAdminSecretName(provider.Name),
			DefaultDatabase: dbcontroller.ProviderDefaultDatabase(provider),
		},
		Network: pginstance.NetworkPolicy{
			OperatorNamespace: cfg.OperatorNamespace,
			AllowedNamespaces: namespaces,
		},
		TLS: pginstance.TLS{
			Enabled:           tlsState.Enabled,
			UsesManagedIssuer: internalTLSUsesManagedIssuer(provider),
			SecretName:        internalTLSSecretName(provider),
			SecretHash:        tlsState.TLSSecretHash,
			IssuerName:        dbcontroller.InternalTLSIssuerName(provider.Name),
			IssuerRef:         internalTLSIssuerRef(provider),
			Certificate: pginstance.Certificate{
				Name: internalTLSCertificateName(provider),
			},
		},
	}

	if tlsState.AdminSecret != nil && tlsState.AdminSecret.Annotations != nil {
		data.Postgres.InstanceHash = tlsState.AdminSecret.Annotations[dbcontroller.InternalAdminSecretKeyAnnotation]
	}

	if provider.Spec.Internal.TLS != nil {
		if duration := provider.Spec.Internal.TLS.Certificate.Duration; duration != nil {
			data.TLS.Certificate.Duration = &pginstance.Duration{String: duration.Duration.String()}
		}
		if renewBefore := provider.Spec.Internal.TLS.Certificate.RenewBefore; renewBefore != nil {
			data.TLS.Certificate.RenewBefore = &pginstance.Duration{String: renewBefore.Duration.String()}
		}
	}

	return data, nil
}

func internalPostgresResources(provided corev1.ResourceRequirements) *corev1.ResourceRequirements {
	resolved := provided.DeepCopy()
	if resolved.Requests == nil {
		resolved.Requests = corev1.ResourceList{}
	}
	if resolved.Limits == nil {
		resolved.Limits = corev1.ResourceList{}
	}

	for name, defaultRequest := range defaultInternalPostgresRequests {
		request, hasRequest := resolved.Requests[name]
		limit, hasLimit := resolved.Limits[name]
		if !hasRequest {
			request = defaultRequest.DeepCopy()
			if hasLimit && request.Cmp(limit) > 0 {
				request = limit.DeepCopy()
			}
			resolved.Requests[name] = request
		}
		if !hasLimit {
			defaultLimit := defaultInternalPostgresLimits[name]
			limit = defaultLimit.DeepCopy()
			if request.Cmp(limit) > 0 {
				limit = request.DeepCopy()
			}
			resolved.Limits[name] = limit
		}
	}

	return resolved
}

func internalTLSEnabled(provider *infraApi.DatabaseProvider) bool {
	return provider != nil && provider.Spec.Internal != nil && provider.Spec.Internal.TLS != nil
}

func internalTLSUsesManagedIssuer(provider *infraApi.DatabaseProvider) bool {
	if !internalTLSEnabled(provider) {
		return false
	}

	ref := provider.Spec.Internal.TLS.IssuerRef
	return ref == nil || ref.Name == ""
}

func internalTLSSecretName(provider *infraApi.DatabaseProvider) string {
	if !internalTLSEnabled(provider) {
		return ""
	}

	if name := provider.Spec.Internal.TLS.Certificate.SecretName; name != "" {
		return name
	}

	return dbcontroller.InternalTLSSecretName(provider.Name)
}

func internalTLSCertificateName(provider *infraApi.DatabaseProvider) string {
	if provider == nil {
		return ""
	}

	return dbcontroller.InternalTLSCertificateName(provider.Name)
}

func internalTLSIssuerRef(provider *infraApi.DatabaseProvider) *infraApi.CertManagerIssuerRef {
	if !internalTLSEnabled(provider) {
		return nil
	}

	if internalTLSUsesManagedIssuer(provider) {
		return &infraApi.CertManagerIssuerRef{
			Name:  dbcontroller.InternalTLSIssuerName(provider.Name),
			Kind:  gvk.CertManagerIssuer.Kind,
			Group: gvk.CertManagerIssuer.Group,
		}
	}

	ref := *provider.Spec.Internal.TLS.IssuerRef
	if ref.Kind == "" {
		ref.Kind = gvk.CertManagerIssuer.Kind
	}
	if ref.Group == "" {
		ref.Group = gvk.CertManagerIssuer.Group
	}

	return &ref
}

func internalTLSStatus(
	provider *infraApi.DatabaseProvider,
	cfg *moduleconfig.Config,
	ready bool,
) *infraApi.ProviderTLSStatus {
	if provider == nil {
		return nil
	}

	return &infraApi.ProviderTLSStatus{
		Enabled:         internalTLSEnabled(provider),
		Ready:           ready,
		Namespace:       dbcontroller.InternalNamespace(provider, cfg.OperatorNamespace),
		IssuerRef:       internalTLSIssuerRef(provider),
		CertificateName: internalTLSCertificateName(provider),
		SecretName:      internalTLSSecretName(provider),
	}
}

// internalTLSSecret relies on Secret reads bypassing the informer cache via
// the manager client's Cache.DisableFor (RHOAIENG-96275): the TLS Secret is
// issued by cert-manager and carries no operator label.
func internalTLSSecret(
	ctx context.Context,
	reader client.Reader,
	provider *infraApi.DatabaseProvider,
	cfg *moduleconfig.Config,
) (*corev1.Secret, error) {
	if !internalTLSEnabled(provider) {
		return nil, nil
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      internalTLSSecretName(provider),
			Namespace: dbcontroller.InternalNamespace(provider, cfg.OperatorNamespace),
		},
	}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(secret), secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("reading internal TLS Secret: %w", err)
	}

	return secret, nil
}

func internalTLSCAData(secret *corev1.Secret) []byte {
	if secret == nil {
		return nil
	}

	if ca := secret.Data[postgres.SecretKeyCA]; len(ca) != 0 {
		return ca
	}

	return secret.Data["tls.crt"]
}

func internalTLSSecretHash(secret *corev1.Secret) string {
	if secret == nil {
		return ""
	}

	h := sha256.New()
	keys := []string{"ca.crt", "tls.crt", "tls.key"}
	for _, key := range keys {
		if value := secret.Data[key]; len(value) != 0 {
			_, _ = h.Write([]byte(key))
			_, _ = h.Write(value)
		}
	}

	return fmt.Sprintf("%x", h.Sum(nil))
}

func resolveInternalTLSState(
	ctx context.Context,
	reader client.Reader,
	provider *infraApi.DatabaseProvider,
	cfg *moduleconfig.Config,
) (TLSState, error) {
	state := TLSState{
		Enabled: internalTLSEnabled(provider),
	}
	if !state.Enabled {
		return state, nil
	}

	secret, err := internalTLSSecret(ctx, reader, provider, cfg)
	if err != nil {
		return state, err
	}
	if secret == nil {
		return state, nil
	}

	state.TLSSecret = secret
	state.CAData = append([]byte(nil), internalTLSCAData(secret)...)
	state.Ready = len(state.CAData) != 0 &&
		len(secret.Data["tls.crt"]) != 0 &&
		len(secret.Data["tls.key"]) != 0
	state.TLSSecretHash = internalTLSSecretHash(secret)

	return state, nil
}

// computeInternalAdminSecret returns the resolved TLS state for the internal
// provider, including the desired admin Secret projection. If the admin Secret
// already exists its password is preserved; otherwise fresh credentials are
// generated. The caller adds TLSState.AdminSecret to rr.Resources so the deploy
// action creates or updates it via SSA. Secret reads bypass the informer cache
// via the manager client's Cache.DisableFor, so a pre-existing admin Secret
// from before the managed label existed is served fresh; the PVC existence
// check reads through the cache, which is safe because the guard only matters
// once the instance has aged past informer sync.
func computeInternalAdminSecret(
	ctx context.Context,
	reader client.Reader,
	provider *infraApi.DatabaseProvider,
	cfg *moduleconfig.Config,
) (TLSState, error) {
	res, err := resolveInternalTLSState(ctx, reader, provider, cfg)
	if err != nil {
		return res, err
	}

	existing := &corev1.Secret{}
	existing.Name = dbcontroller.InternalAdminSecretName(provider.Name)
	existing.Namespace = dbcontroller.InternalNamespace(provider, cfg.OperatorNamespace)

	err = reader.Get(ctx, client.ObjectKeyFromObject(existing), existing)
	if err != nil && !apierrors.IsNotFound(err) {
		return res, err
	}

	password := secretvalue.FromBytes(existing.Data[postgres.SecretKeyPassword])
	if password.IsZero() {
		pvc := &corev1.PersistentVolumeClaim{}
		pvc.Name = dbcontroller.InternalPVCName(provider.Name)
		pvc.Namespace = existing.Namespace
		if err := reader.Get(ctx, client.ObjectKeyFromObject(pvc), pvc); err == nil {
			return res, fmt.Errorf(
				"%w: restore Secret %s/%s with its original password, "+
					"or reset the database admin password before recreating the Secret",
				errInternalAdminCredentialsLost, existing.Namespace, existing.Name,
			)
		} else if !apierrors.IsNotFound(err) {
			return res, fmt.Errorf("checking internal PostgreSQL storage: %w", err)
		}

		pwd, err := postgres.GeneratePassword(24)
		if err != nil {
			return res, fmt.Errorf("generating admin password: %w", err)
		}

		password = pwd
	}

	instanceHash := existing.Annotations[dbcontroller.InternalAdminSecretKeyAnnotation]
	if len(instanceHash) == 0 {
		instanceHash = xid.New().String()
	}

	res.AdminSecret = desiredInternalAdminSecret(provider, cfg, password, res)
	if res.AdminSecret.Annotations == nil {
		res.AdminSecret.Annotations = make(map[string]string, 1)
	}
	res.AdminSecret.Annotations[dbcontroller.InternalAdminSecretKeyAnnotation] = instanceHash

	return res, nil
}

func desiredInternalAdminSecret(
	provider *infraApi.DatabaseProvider,
	cfg *moduleconfig.Config,
	password secretvalue.SecretString,
	tlsState TLSState,
) *corev1.Secret {
	return pginstance.AdminSecret(
		pginstance.Data{
			Namespace: dbcontroller.InternalNamespace(provider, cfg.OperatorNamespace),
			Service: pginstance.Service{
				Name: dbcontroller.InternalServiceName(provider.Name),
			},
			Postgres: pginstance.Postgres{
				AdminSecretName: dbcontroller.InternalAdminSecretName(provider.Name),
				DefaultDatabase: dbcontroller.ProviderDefaultDatabase(provider),
			},
			TLS: pginstance.TLS{
				Enabled: internalTLSEnabled(provider),
			},
		},
		password,
		tlsState.CAData,
	)
}

func referencedClaimNamespaces(
	ctx context.Context,
	cli client.Client,
	provider *infraApi.DatabaseProvider,
) ([]string, error) {
	namespaces := map[string]struct{}{}

	schemaClaims := &infraApi.SchemaClaimList{}
	if err := cli.List(ctx, schemaClaims); err != nil {
		return nil, err
	}
	for i := range schemaClaims.Items {
		claim := &schemaClaims.Items[i]
		if schemaClaimEffectiveProvider(claim) != provider.Name {
			continue
		}
		if conditions.IsStatusConditionTrue(claim, dbcontroller.ConditionProvisioned) {
			namespaces[claim.Namespace] = struct{}{}
		}
	}

	databaseClaims := &infraApi.DatabaseClaimList{}
	if err := cli.List(ctx, databaseClaims); err != nil {
		return nil, err
	}
	for i := range databaseClaims.Items {
		claim := &databaseClaims.Items[i]
		if databaseClaimEffectiveProvider(claim) != provider.Name {
			continue
		}
		if conditions.IsStatusConditionTrue(claim, dbcontroller.ConditionProvisioned) {
			namespaces[claim.Namespace] = struct{}{}
		}
	}

	result := make([]string, 0, len(namespaces))
	for namespace := range namespaces {
		result = append(result, namespace)
	}
	sort.Strings(result)
	return result, nil
}

func schemaClaimEffectiveProvider(claim *infraApi.SchemaClaim) string {
	return dbcontroller.EffectiveProviderName(claim)
}

func databaseClaimEffectiveProvider(claim *infraApi.DatabaseClaim) string {
	return dbcontroller.EffectiveProviderName(claim)
}

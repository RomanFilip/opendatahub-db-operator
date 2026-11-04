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

package v1alpha1

import (
	common "github.com/opendatahub-io/odh-platform-utilities/api/common"
	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	DatabaseProviderKind = "DatabaseProvider"

	DatabaseProviderResource = "databaseproviders"
	DatabaseProviderCRDName  = DatabaseProviderResource + "." + GroupName

	// ConditionOperationalLimitations reports the fixed operational limits of an Internal provider.
	ConditionOperationalLimitations = "OperationalLimitations"
)

// Compile-time interface assertion -- required because DatabaseProvider is
// reconciled via the generic reconciler.ReconcilerFor[T api.PlatformObject]
// builder, same as every other module's CRD types.
var _ fwapi.PlatformObject = (*DatabaseProvider)(nil)

// ProviderType selects which of DatabaseProviderSpec.External/Internal is
// populated. Mutually exclusive with the other, enforced by CEL rules on
// DatabaseProviderSpec below.
type ProviderType string

const (
	// ProviderTypeExternal points at a database instance this service does
	// not own or manage the lifecycle of.
	ProviderTypeExternal ProviderType = "External"

	// ProviderTypeInternal is a controller-managed, single-instance
	// PostgreSQL convenience, not a DBaaS.
	ProviderTypeInternal ProviderType = "Internal"
)

// ExternalProviderSpec points at an admin-managed PostgreSQL instance this
// service checks for connectivity but does not manage.
type ExternalProviderSpec struct {
	// ConnectionSecretRef points at a Secret holding admin-level connection
	// info, which may live in a different namespace than any claim (unlike
	// claim credential Secrets, which always live in the claim's own
	// namespace).
	// +kubebuilder:validation:Required
	ConnectionSecretRef corev1.SecretReference `json:"connectionSecretRef"`

	// Capabilities declares which lifecycle operations claims may perform
	// against this external provider.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:items:Enum=CreateDatabase;CreateSchema
	Capabilities []ExternalCapability `json:"capabilities,omitempty"`
}

type CertManagerIssuerRef struct {
	// Name is the metadata.name of the referenced issuer.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Kind defaults to Issuer when omitted.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Kind string `json:"kind,omitempty"`

	// Group defaults to cert-manager.io when omitted.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Group string `json:"group,omitempty"`
}

// InternalProviderTLSCertificateSpec configures the internal PostgreSQL server
// certificate request managed by cert-manager.
type InternalProviderTLSCertificateSpec struct {
	// SecretName overrides the cert-manager target Secret name.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	SecretName string `json:"secretName,omitempty"`

	// Duration overrides the requested certificate lifetime.
	// +optional
	Duration *metav1.Duration `json:"duration,omitempty"`

	// RenewBefore overrides how long before expiry cert-manager renews.
	// +optional
	RenewBefore *metav1.Duration `json:"renewBefore,omitempty"`
}

// InternalProviderTLSSpec enables TLS for the internal PostgreSQL instance.
// Presence of this block enables TLS; an empty object selects the controller's
// default self-signed issuer and certificate settings.
type InternalProviderTLSSpec struct {
	// IssuerRef overrides the default provider-scoped self-signed issuer.
	// +optional
	IssuerRef *CertManagerIssuerRef `json:"issuerRef,omitempty"`

	// Certificate customizes the server certificate request.
	// +optional
	Certificate InternalProviderTLSCertificateSpec `json:"certificate,omitempty"`
}

// ProviderConnectionStatus is the non-secret connection surface for a
// DatabaseProvider. It intentionally excludes credentials and secret references.
type ProviderConnectionStatus struct {
	Host     string `json:"host,omitempty"`
	Port     int32  `json:"port,omitempty"`
	Database string `json:"database,omitempty"`
}

// StorageSpec configures the Internal provider's PersistentVolumeClaim.
// +kubebuilder:validation:XValidation:rule="quantity(self.size).isGreaterThan(quantity('0'))",message="storage.size must be greater than zero"
// +kubebuilder:validation:XValidation:rule="!quantity(self.size).isLessThan(quantity(oldSelf.size))",message="storage.size can only increase after provider creation"
// +kubebuilder:validation:XValidation:rule="has(oldSelf.storageClassName) == has(self.storageClassName)",message="storageClassName cannot be added or removed after provider creation"
type StorageSpec struct {
	// Size is the requested PVC storage size. It can only increase after creation.
	// +kubebuilder:validation:Required
	Size resource.Quantity `json:"size"`

	// StorageClassName is passed through to the PVC verbatim when set. It cannot
	// be changed after creation because the PVC is retained with its data.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="storageClassName is immutable after provider creation"
	StorageClassName *string `json:"storageClassName,omitempty"`
}

// InternalProviderSpec configures a controller-owned, single-instance
// PostgreSQL StatefulSet. There is deliberately no image
// field -- letting admins point a platform-managed instance at an arbitrary
// image reopens supply-chain and support-surface problems; the image is
// resolved from Extensions via pkg/config compiled defaults (task-08).
// +kubebuilder:validation:XValidation:rule="has(oldSelf.__namespace__) == has(self.__namespace__)",message="namespace cannot be added or removed after provider creation"
// +kubebuilder:validation:XValidation:rule="has(oldSelf.extensions) == has(self.extensions)",message="extensions cannot be added or removed after provider creation"
type InternalProviderSpec struct {
	// Namespace overrides where the internal PostgreSQL resources are created.
	// The operator service account needs the Internal writer role bound there.
	// When unset, the operator namespace is used. Cannot be changed after creation.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="namespace is immutable after provider creation"
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace,omitempty"`

	// Storage configures the instance's PersistentVolumeClaim.
	// +kubebuilder:validation:Required
	Storage StorageSpec `json:"storage"`

	// Resources override the internal PostgreSQL container defaults. Unspecified
	// requests default to 500m CPU and 1Gi memory; limits default to 2 CPU and 4Gi memory.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// TLS enables TLS for the internal PostgreSQL instance. Presence of this
	// field opts into TLS; an empty object uses controller defaults.
	// +optional
	TLS *InternalProviderTLSSpec `json:"tls,omitempty"`

	// Extensions lists PostgreSQL extensions to make available inside the
	// internal instance. Cannot be changed after provider creation.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="extensions are immutable after provider creation"
	// +kubebuilder:validation:items:Enum=vector;pg_trgm;uuid-ossp;pgcrypto
	Extensions []string `json:"extensions,omitempty"`
}

type ProviderTLSStatus struct {
	// Enabled reflects whether TLS is configured for this provider.
	Enabled bool `json:"enabled"`

	// Ready reports whether the provider's TLS contract is fully configured and
	// consumable.
	Ready bool `json:"ready"`

	// Namespace is where the TLS resources for this provider live.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// IssuerRef identifies the issuer used for the current certificate flow.
	// +optional
	IssuerRef *CertManagerIssuerRef `json:"issuerRef,omitempty"`

	// CertificateName is the cert-manager Certificate name, when managed.
	// +optional
	CertificateName string `json:"certificateName,omitempty"`

	// SecretName is the Secret name holding the server certificate material.
	// +optional
	SecretName string `json:"secretName,omitempty"`
}

// DatabaseProviderSpec defines the desired state of DatabaseProvider.
//
// +kubebuilder:validation:XValidation:rule="(has(self.external) ? 1 : 0) + (has(self.internal) ? 1 : 0) == 1",message="exactly one of external or internal must be set"
// +kubebuilder:validation:XValidation:rule="self.type == 'External' ? has(self.external) : has(self.internal)",message="spec.type must match the set provider type"
type DatabaseProviderSpec struct {
	// Type selects which of External/Internal below is populated.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=External;Internal
	Type ProviderType `json:"type"`

	// DefaultDatabase is the database claims use when they do not specify one.
	// For external providers, this also supplies the default database when the
	// admin Secret omits pg.database. When unset, the provider Secret (external)
	// or the built-in internal default database may still supply the value.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	DefaultDatabase string `json:"defaultDatabase,omitempty"`

	// External configures a provider pointing at an admin-managed instance.
	// Mutually exclusive with Internal.
	// +optional
	External *ExternalProviderSpec `json:"external,omitempty"`

	// Internal configures a controller-owned instance. Mutually exclusive
	// with External.
	// +optional
	Internal *InternalProviderSpec `json:"internal,omitempty"`
}

// DatabaseProviderStatus defines the observed state of DatabaseProvider.
// Its embedded common.Status provides Phase, Conditions, and
// ObservedGeneration.
type DatabaseProviderStatus struct {
	fwapi.Status                  `json:",inline"`
	common.ComponentReleaseStatus `json:",inline"`

	// Connection is the non-secret connection surface for the provider.
	// +optional
	Connection ProviderConnectionStatus `json:"connection,omitempty"`

	// TLS reports the resolved TLS state for the provider.
	// +optional
	TLS *ProviderTLSStatus `json:"tls,omitempty"`

	// ServerVersion is the detected PostgreSQL server version. It does not gate binding.
	// +optional
	ServerVersion string `json:"serverVersion,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`,description="Provider type"
// +kubebuilder:printcolumn:name="Host",type=string,JSONPath=`.status.connection.host`,description="Resolved host"
// +kubebuilder:printcolumn:name="Database",type=string,JSONPath=`.status.connection.database`,description="Resolved admin database"
// +kubebuilder:printcolumn:name="Reachable",type=string,JSONPath=`.status.conditions[?(@.type=="Reachable")].status`,description="Reachable"
// +kubebuilder:printcolumn:name="Server Version",type=string,JSONPath=`.status.serverVersion`,description="Detected PostgreSQL server version"
// Internal names also seed ServiceAccount ({name}-postgres) and SCC RoleBinding
// ({name}-postgres-scc). Cap at 50 so those derived names stay within the
// 63-character Kubernetes limit (see controller.MaxInternalProviderNameLength).
// +kubebuilder:validation:XValidation:rule="self.spec.type != 'Internal' || (size(self.metadata.name) <= 50 && self.metadata.name.matches('^[a-z]$|^[a-z]([-a-z0-9]*[a-z0-9])$'))",message="Internal provider metadata.name must be a DNS-1035 label of at most 50 characters so derived ServiceAccount and RoleBinding names stay within the 63-character Kubernetes limit"

// DatabaseProvider is the Schema for the databaseproviders API.
type DatabaseProvider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DatabaseProviderSpec   `json:"spec,omitempty"`
	Status DatabaseProviderStatus `json:"status,omitempty"`
}

func (c *DatabaseProvider) GetStatus() *fwapi.Status {
	return &c.Status.Status
}

func (c *DatabaseProvider) GetConditions() []fwapi.Condition {
	return c.Status.GetConditions()
}

func (c *DatabaseProvider) SetConditions(conditions []fwapi.Condition) {
	c.Status.SetConditions(conditions)
}

func (c *DatabaseProvider) GetReleaseStatus() *common.ComponentReleaseStatus {
	return &c.Status.ComponentReleaseStatus
}

func (c *DatabaseProvider) SetReleaseStatus(status common.ComponentReleaseStatus) {
	c.Status.ComponentReleaseStatus = status
}

// +kubebuilder:object:root=true

// DatabaseProviderList contains a list of DatabaseProvider.
type DatabaseProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DatabaseProvider `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &DatabaseProvider{}, &DatabaseProviderList{})
		return nil
	})
}

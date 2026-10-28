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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	DatabaseClaimKind = "DatabaseClaim"

	DatabaseClaimResource = "databaseclaims"
	DatabaseClaimCRDName  = DatabaseClaimResource + "." + GroupName
)

// Compile-time interface assertion -- required because DatabaseClaim is
// reconciled via the generic reconciler.ReconcilerFor[T api.PlatformObject]
// builder (docs/plan.md §6), same as every other module's CRD types.
var _ fwapi.PlatformObject = (*DatabaseClaim)(nil)

// DatabaseConnectionStatus is the resolved connection surfaced to consumers
// once a DatabaseClaim is Provisioned (docs/plan.md §5). Deliberately not
// SchemaConnectionStatus: spec.md's DatabaseClaim status example has no
// "schema" key under connection at all, so this is its own, smaller type
// rather than a shared type with an always-empty field.
type DatabaseConnectionStatus struct {
	// SecretRef names the credentials Secret in the claim's own namespace.
	// When spec.secretName is set, it matches that value; otherwise it falls
	// back to the claim's own metadata.name.
	SecretRef corev1.LocalObjectReference `json:"secretRef,omitempty"`
	Host      string                      `json:"host,omitempty"`
	Port      int32                       `json:"port,omitempty"`
	Database  string                      `json:"database,omitempty"`
}

// DatabaseClaimSpec defines the desired state of DatabaseClaim.
//
// Field-level self == oldSelf rules do not run when an optional field is
// removed or when it is first added; these Spec-level presence rules close
// both gaps so a field cannot be cleared or introduced after create (same
// pattern as SchemaClaimSpec).
//
// +kubebuilder:validation:XValidation:rule="has(oldSelf.secretName) == has(self.secretName)",message="secretName cannot be added or removed after claim creation"
// +kubebuilder:validation:XValidation:rule="has(oldSelf.database) == has(self.database)",message="database cannot be added or removed after claim creation"
// +kubebuilder:validation:XValidation:rule="(!has(oldSelf.provider) || (!has(oldSelf.provider.name) && !has(oldSelf.provider.selector))) == (!has(self.provider) || (!has(self.provider.name) && !has(self.provider.selector)))",message="provider selection cannot be added or removed after claim creation"
type DatabaseClaimSpec struct {
	// Provider selects the DatabaseProvider to provision against. Omission
	// selects the DatabaseService default provider.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="provider is immutable once set"
	Provider ProviderRef `json:"provider,omitempty"`

	// SecretName overrides the name of the credentials Secret projected in the
	// claim namespace. If omitted, the claim name is used. Immutable once set so
	// updates never strand credentials under an old Secret name.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="secretName is immutable once set"
	SecretName string `json:"secretName,omitempty"`

	// Database selects the target database for the claim. When omitted, the
	// provider's default database is used. When set, the controller may create
	// the database if it does not already exist, subject to provider
	// capabilities. Immutable once set: changing it mid-life implies a
	// different claim, not an update to this one.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="database is immutable once set"
	Database string `json:"database,omitempty"`

	// Access is the privilege level granted to the provisioned user.
	// +kubebuilder:validation:Enum=ReadWrite;ReadOnly
	// +kubebuilder:default=ReadWrite
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="access is immutable once set"
	Access AccessMode `json:"access,omitempty"`

	// DeletionPolicy governs database lifecycle on claim deletion. Retain
	// (default) drops only the claim role; Delete may drop a named database
	// the claim created, but never the provider's shared default database.
	// +kubebuilder:validation:Enum=Retain;Delete
	// +kubebuilder:default=Retain
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
}

// DatabaseClaimStatus defines the observed state of DatabaseClaim. Phase
// (human-readable summary only -- consumers must gate on
// conditions[type=Provisioned], never on Phase) comes from the embedded
// common.Status, not a redundant field here.
type DatabaseClaimStatus struct {
	fwapi.Status                  `json:",inline"`
	common.ComponentReleaseStatus `json:",inline"`

	// Database is the effective database name once provisioning resolves it:
	// spec.database when set, otherwise the provider default.
	Database string `json:"database,omitempty"`

	// DatabaseCreated is true when this claim created the named database during
	// provisioning. DeletionPolicy=Delete may drop the database only when this
	// is true (and no other DatabaseClaim still references it).
	// +optional
	DatabaseCreated bool `json:"databaseCreated,omitempty"`

	// Connection is the resolved connection surface once Provisioned,
	// including the effective credentials Secret name.
	// +optional
	Connection DatabaseConnectionStatus `json:"connection,omitempty"`

	// Provider is the single DatabaseProvider ultimately selected when
	// spec.provider.selector matched more than one candidate (highest
	// selection-priority annotation, ties broken alphabetically by name --
	// docs/plan.md §6). Only the winner is surfaced: a claim binds to exactly
	// one provider, so there's nothing to gain from also listing the
	// candidates that lost.
	// +optional
	Provider string `json:"provider,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Database",type=string,JSONPath=`.status.database`,description="Database"
// +kubebuilder:printcolumn:name="Provisioned",type=string,JSONPath=`.status.conditions[?(@.type=="Provisioned")].status`,description="Provisioned"

// DatabaseClaim is the Schema for the databaseclaims API.
type DatabaseClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DatabaseClaimSpec   `json:"spec,omitempty"`
	Status DatabaseClaimStatus `json:"status,omitempty"`
}

func (c *DatabaseClaim) GetStatus() *fwapi.Status {
	return &c.Status.Status
}

func (c *DatabaseClaim) GetConditions() []fwapi.Condition {
	return c.Status.GetConditions()
}

func (c *DatabaseClaim) SetConditions(conditions []fwapi.Condition) {
	c.Status.SetConditions(conditions)
}

func (c *DatabaseClaim) GetReleaseStatus() *common.ComponentReleaseStatus {
	return &c.Status.ComponentReleaseStatus
}

func (c *DatabaseClaim) SetReleaseStatus(status common.ComponentReleaseStatus) {
	c.Status.ComponentReleaseStatus = status
}

// +kubebuilder:object:root=true

// DatabaseClaimList contains a list of DatabaseClaim.
type DatabaseClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DatabaseClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &DatabaseClaim{}, &DatabaseClaimList{})
		return nil
	})
}

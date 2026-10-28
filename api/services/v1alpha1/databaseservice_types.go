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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	// DatabaseServiceInstanceName is the singleton instance name enforced by
	// the CEL rule below.
	DatabaseServiceInstanceName = "default-db-operator"
)

var _ fwapi.PlatformObject = (*DatabaseService)(nil)

// DatabaseServiceSpec is empty because DatabaseService has no configurable desired state.
type DatabaseServiceSpec struct {
}

// DatabaseServiceStatus defines the observed state of DatabaseService.
type DatabaseServiceStatus struct {
	fwapi.Status                  `json:",inline"`
	common.ComponentReleaseStatus `json:",inline"`
	// DefaultProviderName records the install-time name so later configuration
	// changes cannot silently redirect providerless claims.
	DefaultProviderName string `json:"defaultProviderName,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'default-db-operator'",message="DatabaseService name must be default-db-operator"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`,description="Whether the DatabaseService is ready"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`,description="Reason the DatabaseService is not ready"
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.releases[?(@.name=="platform")].version`,description="Module Version"

// DatabaseService is the Schema for the databaseservices API.
type DatabaseService struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DatabaseServiceSpec   `json:"spec,omitempty"`
	Status DatabaseServiceStatus `json:"status,omitempty"`
}

// GetStatus implements fwapi.WithStatus.
func (c *DatabaseService) GetStatus() *fwapi.Status {
	return &c.Status.Status
}

// GetConditions implements fwapi.ConditionsAccessor.
func (c *DatabaseService) GetConditions() []fwapi.Condition {
	return c.Status.GetConditions()
}

// SetConditions implements fwapi.ConditionsAccessor.
func (c *DatabaseService) SetConditions(conditions []fwapi.Condition) {
	c.Status.SetConditions(conditions)
}

// GetReleaseStatus implements common.WithReleases.
func (c *DatabaseService) GetReleaseStatus() *common.ComponentReleaseStatus {
	return &c.Status.ComponentReleaseStatus
}

// SetReleaseStatus implements common.WithReleases.
func (c *DatabaseService) SetReleaseStatus(status common.ComponentReleaseStatus) {
	c.Status.ComponentReleaseStatus = status
}

// +kubebuilder:object:root=true

// DatabaseServiceList is returned by LIST requests for DatabaseService.
type DatabaseServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DatabaseService `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &DatabaseService{}, &DatabaseServiceList{})
		return nil
	})
}

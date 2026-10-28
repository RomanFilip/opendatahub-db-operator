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

// Package gvk centralizes GroupVersionKind constants for the
// opendatahub-db-operator module. This is the only place module code may
// import the platform's cluster-scoped GVK package
// (odh-platform-utilities/framework/cluster/gvk) directly.
//
// The DatabaseService kind needs no entry here because chart generation does
// not special-case it.
package gvk

import (
	fwgvk "github.com/opendatahub-io/odh-platform-utilities/framework/cluster/gvk"
	infraApi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Generic Kubernetes GVKs reused from the platform's cluster-scoped GVK
// package. chartgen (cmd/chartgen) dispatches its per-resource template
// transformations on these.
var (
	Namespace                      = fwgvk.Namespace
	Deployment                     = fwgvk.Deployment
	ServiceAccount                 = fwgvk.ServiceAccount
	ConfigMap                      = fwgvk.ConfigMap
	ClusterRoleBinding             = fwgvk.ClusterRoleBinding
	RoleBinding                    = fwgvk.RoleBinding
	MutatingWebhookConfiguration   = fwgvk.MutatingWebhookConfiguration
	ValidatingWebhookConfiguration = fwgvk.ValidatingWebhookConfiguration
	CertManagerIssuer              = fwgvk.CertManagerIssuer
	CertManagerCertificate         = fwgvk.CertManagerCertificate
	Secret                         = fwgvk.Secret
)

var (
	DatabaseProvider     = infraApi.SchemeGroupVersion.WithKind("DatabaseProvider")
	DatabaseProviderList = infraApi.SchemeGroupVersion.WithKind("DatabaseProviderList")
	DatabaseClaim        = infraApi.SchemeGroupVersion.WithKind("DatabaseClaim")
	SchemaClaim          = infraApi.SchemeGroupVersion.WithKind("SchemaClaim")

	// SecurityContextConstraints is the OpenShift SCC kind. Absent on Kind.
	SecurityContextConstraints = schema.GroupVersionKind{
		Group:   "security.openshift.io",
		Version: "v1",
		Kind:    "SecurityContextConstraints",
	}
)

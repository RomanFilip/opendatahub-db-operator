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
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	servicesv1alpha1 "github.com/opendatahub-io/opendatahub-db-operator/api/services/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestCleanupDatabaseServiceLeavesUnownedSingleton(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	g.Expect(servicesv1alpha1.AddToScheme(scheme)).To(Succeed())

	singleton := &servicesv1alpha1.DatabaseService{
		ObjectMeta: metav1.ObjectMeta{Name: servicesv1alpha1.DatabaseServiceInstanceName},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(singleton).Build()

	g.Expect(cleanupDatabaseServiceWithClient(context.Background(), kubeClient)).To(Succeed())

	actual := &servicesv1alpha1.DatabaseService{}
	g.Expect(kubeClient.Get(context.Background(), client.ObjectKeyFromObject(singleton), actual)).To(Succeed())
}

func TestCleanupNamespaceLeavesUnownedNamespace(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())

	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "keep-me"}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespace).Build()

	g.Expect(cleanupNamespaceWithClient(context.Background(), kubeClient, namespace.Name)).To(HaveOccurred())

	actual := &corev1.Namespace{}
	g.Expect(kubeClient.Get(context.Background(), client.ObjectKeyFromObject(namespace), actual)).To(Succeed())
}

func TestPreflightAllowsFreshClusterWithoutDatabaseServiceAPI(t *testing.T) {
	noDatabaseAPI := &meta.NoKindMatchError{
		GroupKind: schema.GroupKind{Group: servicesv1alpha1.GroupVersion.Group, Kind: "DatabaseService"},
	}
	kubeClient := lifecycleClient(t, nil, noDatabaseAPI, nil)

	g := NewWithT(t)
	g.Expect(preflightWithClient(context.Background(), kubeClient, "new-e2e", "e2e-release")).To(Succeed())
}

func TestPreflightRejectsExistingDatabaseService(t *testing.T) {
	for _, owner := range []string{"foreign", e2eOwnerValue} {
		t.Run(owner, func(t *testing.T) {
			labels := map[string]string{}
			if owner == e2eOwnerValue {
				labels[e2eOwnerLabel] = owner
			}
			singleton := &servicesv1alpha1.DatabaseService{
				ObjectMeta: metav1.ObjectMeta{Name: servicesv1alpha1.DatabaseServiceInstanceName, Labels: labels},
			}
			kubeClient := lifecycleClient(t, []client.Object{singleton}, nil, nil)

			g := NewWithT(t)
			g.Expect(preflightWithClient(context.Background(), kubeClient, "new-e2e", "e2e-release")).To(HaveOccurred())
		})
	}
}

func TestPreflightRejectsForeignOrTerminatingNamespace(t *testing.T) {
	for _, tc := range []struct {
		name      string
		namespace *corev1.Namespace
	}{
		{
			name: "foreign namespace",
			namespace: &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
				Name: "e2e", Labels: map[string]string{"owner": "someone-else"},
			}},
		},
		{
			name: "terminating owned namespace",
			namespace: &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
				Name: "e2e", Labels: map[string]string{e2eOwnerLabel: e2eOwnerValue},
				DeletionTimestamp: &metav1.Time{Time: metav1.Now().Time},
				Finalizers:        []string{"test.example/finalizer"},
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kubeClient := lifecycleClient(t, []client.Object{tc.namespace}, nil, nil)
			g := NewWithT(t)
			g.Expect(preflightWithClient(context.Background(), kubeClient, tc.namespace.Name, "e2e-release")).To(HaveOccurred())
		})
	}
}

func TestPreflightChecksEveryHelmReleaseRevision(t *testing.T) {
	for _, tc := range []struct {
		name    string
		labels  map[string]string
		wantErr bool
	}{
		{
			name: "marked revision",
			labels: map[string]string{
				"owner": "helm", "name": "e2e-release", "version": "1",
				e2eOwnerLabel: e2eOwnerValue,
			},
		},
		{
			name:    "unmarked revision",
			labels:  map[string]string{"owner": "helm", "name": "e2e-release", "version": "2"},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			namespace := ownedNamespace("e2e")
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace.Name, Name: "sh.helm.release.v1.e2e-release.v" + tc.labels["version"],
				Labels: tc.labels,
			}}
			kubeClient := lifecycleClient(t, []client.Object{namespace, secret}, nil, nil)
			err := preflightWithClient(context.Background(), kubeClient, namespace.Name, "e2e-release")
			g := NewWithT(t)
			if tc.wantErr {
				g.Expect(err).To(HaveOccurred())
			} else {
				g.Expect(err).NotTo(HaveOccurred())
			}
		})
	}
}

func TestPreflightRejectsAnUnownedRevisionAlongsideOwnedRevision(t *testing.T) {
	namespace := ownedNamespace("e2e")
	marked := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace.Name, Name: "sh.helm.release.v1.e2e-release.v1",
		Labels: map[string]string{
			"owner": "helm", "name": "e2e-release", "version": "1",
			e2eOwnerLabel: e2eOwnerValue,
		},
	}}
	unmarked := marked.DeepCopy()
	unmarked.Name = "sh.helm.release.v1.e2e-release.v2"
	unmarked.Labels["version"] = "2"
	delete(unmarked.Labels, e2eOwnerLabel)
	kubeClient := lifecycleClient(t, []client.Object{namespace, marked, unmarked}, nil, nil)

	g := NewWithT(t)
	g.Expect(preflightWithClient(context.Background(), kubeClient, namespace.Name, "e2e-release")).To(HaveOccurred())
}

func TestCheckPrerequisitesRejectsMissingCertManagerAPIsAndReadErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    string
		listErr error
	}{
		{name: "Certificate API missing", kind: "Certificate", listErr: noKindMatch("cert-manager.io", "Certificate")},
		{name: "Issuer API missing", kind: "Issuer", listErr: noKindMatch("cert-manager.io", "Issuer")},
		{name: "forbidden", kind: "Certificate", listErr: apierrors.NewForbidden(schema.GroupResource{
			Group: "cert-manager.io", Resource: "certificates",
		}, "e2e", errors.New("forbidden"))},
		{name: "discovery failure", kind: "Issuer", listErr: errors.New("discovery unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kubeClient := lifecycleClient(t, []client.Object{ownedNamespace("e2e")}, nil, map[string]error{tc.kind: tc.listErr})
			g := NewWithT(t)
			g.Expect(checkPrerequisitesWithClient(context.Background(), kubeClient, "e2e")).To(HaveOccurred())
		})
	}
}

func TestCheckPrerequisitesRejectsForeignNamespace(t *testing.T) {
	kubeClient := lifecycleClient(t, []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "e2e"}},
	}, nil, nil)

	g := NewWithT(t)
	g.Expect(checkPrerequisitesWithClient(context.Background(), kubeClient, "e2e")).To(HaveOccurred())
}

func TestDiagnosticsAreBoundedAndExcludeSecretDataAndEventMessages(t *testing.T) {
	scheme := runtime.NewScheme()
	g := NewWithT(t)
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
	g.Expect(appsv1.AddToScheme(scheme)).To(Succeed())
	objects := []client.Object{
		ownedNamespace("e2e"),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "sensitive-secret-name", Namespace: "e2e"},
			Data: map[string][]byte{"password": []byte("diagnostics-secret-value")}},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "pod-event", Namespace: "e2e"},
			Reason: "Failed", Message: "sensitive-event-message", InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "db-0"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "db-0", Namespace: "e2e"},
			Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
				Name: "database", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
			}}}},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	var output bytes.Buffer
	g.Expect(diagnosticsWithClient(context.Background(), kubeClient, "e2e", &output)).To(Succeed())
	text := output.String()
	g.Expect(text).To(ContainSubstring("pod=db-0 phase=Pending"))
	g.Expect(text).To(ContainSubstring("reason=Failed"))
	g.Expect(text).To(ContainSubstring("state=waiting:ImagePullBackOff"))
	g.Expect(text).NotTo(ContainSubstring("sensitive-secret-name"))
	g.Expect(text).NotTo(ContainSubstring("diagnostics-secret-value"))
	g.Expect(text).NotTo(ContainSubstring("sensitive-event-message"))
}

func TestAuthorizeTeardownWithClientOwnershipCases(t *testing.T) {
	markedRelease := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: "e2e", Name: "helm-release-v1",
		Labels: map[string]string{
			"owner": "helm", "name": "e2e-release", e2eOwnerLabel: e2eOwnerValue,
		},
	}}
	unmarkedRelease := markedRelease.DeepCopy()
	unmarkedRelease.Name = "helm-release-v2"
	delete(unmarkedRelease.Labels, e2eOwnerLabel)

	ownedSingleton := &servicesv1alpha1.DatabaseService{ObjectMeta: metav1.ObjectMeta{
		Name:   servicesv1alpha1.DatabaseServiceInstanceName,
		Labels: map[string]string{e2eOwnerLabel: e2eOwnerValue},
	}}
	foreignSingleton := ownedSingleton.DeepCopy()
	delete(foreignSingleton.Labels, e2eOwnerLabel)

	for _, tc := range []struct {
		name    string
		objects []client.Object
		want    bool
		wantErr bool
	}{
		{
			name: "foreign namespace with same-name release",
			objects: []client.Object{
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "e2e"}}, markedRelease,
			},
			wantErr: true,
		},
		{
			name:    "unmarked release in owned namespace",
			objects: []client.Object{ownedNamespace("e2e"), unmarkedRelease},
			wantErr: true,
		},
		{
			name:    "owned namespace and singleton without release",
			objects: []client.Object{ownedNamespace("e2e"), ownedSingleton},
			want:    true,
		},
		{
			name:    "foreign singleton in owned namespace",
			objects: []client.Object{ownedNamespace("e2e"), foreignSingleton},
			wantErr: true,
		},
		{
			name:    "owned orphan singleton",
			objects: []client.Object{ownedSingleton},
			wantErr: true,
		},
		{
			name: "entire installation absent",
			want: false,
		},
		{
			name:    "owned namespace without singleton or release",
			objects: []client.Object{ownedNamespace("e2e")},
			want:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kubeClient := lifecycleClient(t, tc.objects, nil, nil)
			authorized, err := authorizeTeardownWithClient(context.Background(), kubeClient, "e2e", "e2e-release")
			g := NewWithT(t)
			if tc.wantErr {
				g.Expect(err).To(HaveOccurred())
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(authorized).To(Equal(tc.want))
		})
	}
}

func TestPreflightDoesNotTreatDatabaseServiceAuthorizationFailureAsAbsence(t *testing.T) {
	for _, err := range []error{
		apierrors.NewForbidden(schema.GroupResource{Group: servicesv1alpha1.GroupVersion.Group, Resource: "databaseservices"},
			"default-db-operator", errors.New("forbidden")),
		errors.New("discovery unavailable"),
	} {
		kubeClient := lifecycleClient(t, nil, err, nil)
		g := NewWithT(t)
		g.Expect(preflightWithClient(context.Background(), kubeClient, "new-e2e", "e2e-release")).To(HaveOccurred())
	}
}

func lifecycleClient(t *testing.T, objects []client.Object, databaseServiceGetErr error, listErrors map[string]error) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := servicesv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*servicesv1alpha1.DatabaseService); ok && databaseServiceGetErr != nil {
				return databaseServiceGetErr
			}
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if unstructuredList, ok := list.(*unstructured.UnstructuredList); ok {
				kind := strings.TrimSuffix(unstructuredList.GroupVersionKind().Kind, "List")
				if err := listErrors[kind]; err != nil {
					return err
				}
				return nil
			}
			return c.List(ctx, list, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			return c.Delete(ctx, obj, opts...)
		},
	})
}

func ownedNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: name, Labels: map[string]string{e2eOwnerLabel: e2eOwnerValue},
	}}
}

func noKindMatch(group, kind string) error {
	return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: group, Kind: kind}}
}

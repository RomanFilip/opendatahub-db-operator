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

package support

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestWaitNamespaceDeletion(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())

	// newClient builds a fake client whose every Get is served by get(); a
	// nil namespace with a nil error is served as a bare empty object.
	newClient := func(get func() (*corev1.Namespace, error)) client.Client {
		return fake.NewClientBuilder().WithScheme(scheme).
			WithInterceptorFuncs(interceptor.Funcs{Get: func(
				_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption,
			) error {
				found, err := get()
				if err != nil {
					return err
				}
				if found != nil {
					*obj.(*corev1.Namespace) = *found
				}
				return nil
			}}).
			Build()
	}

	t.Run("namespace gone returns nil", func(t *testing.T) {
		g := NewWithT(t)
		cli := newClient(func() (*corev1.Namespace, error) {
			return nil, apierrors.NewNotFound(corev1.Resource("namespaces"), "gone")
		})
		g.Expect(WaitNamespaceDeletion(context.Background(), cli, "gone", "uid-1")).To(Succeed())
	})

	t.Run("same-named namespace with a different uid counts as gone", func(t *testing.T) {
		g := NewWithT(t)
		cli := newClient(func() (*corev1.Namespace, error) {
			return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{UID: types.UID("uid-other")}}, nil
		})
		g.Expect(WaitNamespaceDeletion(context.Background(), cli, "swapped", "uid-1")).To(Succeed())
	})

	t.Run("namespace stuck terminating until the context expires", func(t *testing.T) {
		g := NewWithT(t)
		cli := newClient(func() (*corev1.Namespace, error) {
			return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{UID: types.UID("uid-1")}}, nil
		})
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		err := WaitNamespaceDeletion(ctx, cli, "stuck", "uid-1")
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("still terminating"))
	})

	t.Run("nil client errors", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(WaitNamespaceDeletion(context.Background(), nil, "any", "uid-1")).NotTo(Succeed())
	})
}

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

package controller

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infraApi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
)

// BroadcastListMapper returns a map function that enqueues every object in the
// provided list type whenever the watched dependency changes.
//
// List failures cannot be returned from MapFunc; they are logged and the
// event is dropped. Dependents still heal via their default requeue.
func BroadcastListMapper(
	cli client.Client,
	prototype client.ObjectList,
) handler.MapFunc {
	log := ctrl.Log.WithName("broadcast-list-mapper").WithValues("listType", fmt.Sprintf("%T", prototype))
	return func(ctx context.Context, _ client.Object) []reconcile.Request {
		list, ok := prototype.DeepCopyObject().(client.ObjectList)
		if !ok {
			log.Error(nil, "prototype is not a client.ObjectList")
			return nil
		}
		if err := cli.List(ctx, list); err != nil {
			log.Error(err, "failed to list dependents for broadcast enqueue")
			return nil
		}

		items, err := metav1.ExtractList(list)
		if err != nil {
			log.Error(err, "failed to extract dependents for broadcast enqueue")
			return nil
		}

		reqs := make([]reconcile.Request, 0, len(items))
		for _, item := range items {
			obj, ok := item.(client.Object)
			if !ok {
				continue
			}
			reqs = append(reqs, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(obj),
			})
		}
		return reqs
	}
}

// providerRefNames returns the provider names a claim references in its spec
// and records as bound in its status. Handles typed and unstructured objects.
func providerRefNames(obj client.Object) (specName, statusName string) {
	switch claim := obj.(type) {
	case *infraApi.DatabaseClaim:
		return claim.Spec.Provider.Name, claim.Status.Provider
	case *infraApi.SchemaClaim:
		return claim.Spec.Provider.Name, claim.Status.Provider
	case *unstructured.Unstructured:
		specName, _, _ := unstructured.NestedString(claim.Object, "spec", "provider", "name")
		statusName, _, _ := unstructured.NestedString(claim.Object, "status", "provider")
		return specName, statusName
	default:
		return "", ""
	}
}

// EffectiveProviderName returns the DatabaseProvider a claim is bound to or
// requesting: spec.provider.name when set, otherwise status.provider.
func EffectiveProviderName(obj client.Object) string {
	specName, statusName := providerRefNames(obj)
	if specName != "" {
		return specName
	}
	return statusName
}

// ClaimWatchEventHandler enqueues the providers a claim event affects so
// Internal NetworkPolicy ingress refreshes without waiting for the periodic
// requeue (RHOAIENG-96275). Updates enqueue the union of old and new provider
// names because a rebinding or unbinding selector claim leaves the abandoned
// provider discoverable only in the previous object. Over-enqueueing is safe;
// the watch predicates filter resync noise.
func ClaimWatchEventHandler() handler.EventHandler {
	type claimQueue = workqueue.TypedRateLimitingInterface[reconcile.Request]
	enqueue := func(q claimQueue, objects ...client.Object) {
		seen := make(map[reconcile.Request]struct{})
		for _, obj := range objects {
			if obj == nil {
				continue
			}
			specName, statusName := providerRefNames(obj)
			for _, name := range []string{specName, statusName} {
				if name == "" {
					continue
				}
				req := reconcile.Request{NamespacedName: types.NamespacedName{Name: name}}
				if _, dup := seen[req]; dup {
					continue
				}
				seen[req] = struct{}{}
				q.Add(req)
			}
		}
	}
	return handler.Funcs{
		CreateFunc: func(_ context.Context, e event.TypedCreateEvent[client.Object], q claimQueue) {
			enqueue(q, e.Object)
		},
		UpdateFunc: func(_ context.Context, e event.TypedUpdateEvent[client.Object], q claimQueue) {
			enqueue(q, e.ObjectOld, e.ObjectNew)
		},
		DeleteFunc: func(_ context.Context, e event.TypedDeleteEvent[client.Object], q claimQueue) {
			enqueue(q, e.Object)
		},
		// Generic events carry no state transition; the periodic requeue covers them.
	}
}

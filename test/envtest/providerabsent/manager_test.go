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

// Package providerabsent runs in a separate test process because controller-runtime
// rejects duplicate controller names across managers in one process.
package providerabsent

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/gomega"
	"github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
	servicesv1alpha1 "github.com/opendatahub-io/opendatahub-db-operator/api/services/v1alpha1"
	moduleconfig "github.com/opendatahub-io/opendatahub-db-operator/pkg/config"
	modulemanager "github.com/opendatahub-io/opendatahub-db-operator/pkg/manager"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestDatabaseServiceReconcilesWithoutProviderCRD(t *testing.T) {
	g := NewWithT(t)
	ctrl.SetLogger(logr.Discard())

	env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "..", "config", "crd", "bases", "services.platform.opendatahub.io_databaseservices.yaml"),
		},
		ErrorIfCRDPathMissing: true,
	}
	restCfg, err := env.Start()
	g.Expect(err).NotTo(HaveOccurred())
	t.Cleanup(func() { g.Expect(env.Stop()).To(Succeed()) })

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	cfg, err := moduleconfig.Load()
	g.Expect(err).NotTo(HaveOccurred())
	cfg.Controller.LeaderElection.Enabled = false
	cfg.Controller.Metrics.BindAddress = "0"
	cfg.Controller.Health.BindAddress = "0"
	cfg.Controller.Pprof.BindAddress = "0"
	cfg.Controller.Webhook.Enabled = false

	mgr, err := modulemanager.New(ctx, restCfg, cfg)
	g.Expect(err).NotTo(HaveOccurred())

	managerDone := make(chan struct{})
	var managerErr error
	go func() {
		managerErr = mgr.Start(ctx)
		close(managerDone)
	}()
	t.Cleanup(func() {
		cancel()
		<-managerDone
		if managerErr != nil {
			t.Errorf("manager failed while stopping: %v", managerErr)
		}
	})

	instance := &servicesv1alpha1.DatabaseService{
		ObjectMeta: metav1.ObjectMeta{Name: servicesv1alpha1.DatabaseServiceInstanceName},
	}
	g.Expect(mgr.GetClient().Create(ctx, instance)).To(Succeed())

	g.Eventually(func(g Gomega) {
		select {
		case <-managerDone:
			g.Expect(managerDone).NotTo(BeClosed(), "manager stopped unexpectedly: %v", managerErr)
		default:
		}

		got := &servicesv1alpha1.DatabaseService{}
		g.Expect(mgr.GetClient().Get(ctx, client.ObjectKeyFromObject(instance), got)).To(Succeed())
		condition := conditions.FindStatusCondition(got, "DefaultProviderReady")
		g.Expect(condition).NotTo(BeNil())
		g.Expect(condition.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(condition.Reason).To(Equal("ProviderAPIUnavailable"))
	}, 30*time.Second).Should(Succeed())

	g.Expect(mgr.GetClient().Delete(ctx, instance)).To(Succeed())
	g.Eventually(func() bool {
		err := mgr.GetClient().Get(ctx, client.ObjectKeyFromObject(instance), &servicesv1alpha1.DatabaseService{})
		return apierrors.IsNotFound(err)
	}, 30*time.Second).Should(BeTrue())
}

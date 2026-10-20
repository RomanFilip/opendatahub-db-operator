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

package e2e

import (
	"os"
	"testing"

	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestCertManagerProfile(t *testing.T) {
	profile := os.Getenv("ODH_E2E_CERT_MANAGER_PROFILE")
	if profile == "" {
		t.Skip("set ODH_E2E_CERT_MANAGER_PROFILE to present or absent")
	}
	if profile != certManagerProfilePresent && profile != certManagerProfileAbsent {
		t.Fatalf("unknown cert-manager profile %q", profile)
	}

	g := NewWithT(t)
	cfg, err := ctrl.GetConfig()
	g.Expect(err).NotTo(HaveOccurred())
	scheme := runtime.NewScheme()
	g.Expect(apiextensionsv1.AddToScheme(scheme)).To(Succeed())
	cli, err := client.New(cfg, client.Options{Scheme: scheme})
	g.Expect(err).NotTo(HaveOccurred())

	for _, name := range []string{"certificates.cert-manager.io", "issuers.cert-manager.io"} {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		err := cli.Get(t.Context(), client.ObjectKey{Name: name}, crd)
		if profile == certManagerProfilePresent {
			g.Expect(err).NotTo(HaveOccurred(), "%s must be installed", name)
			g.Expect(crd.Status.Conditions).To(ContainElement(And(
				HaveField("Type", apiextensionsv1.Established),
				HaveField("Status", apiextensionsv1.ConditionTrue),
			)), "%s must be established", name)
		} else {
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "%s must be absent: %v", name, err)
		}
	}
}

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

package operands

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/opendatahub-io/odh-platform-utilities/framework/render/kustomize"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	infraApi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
)

// embeddedCRDs lists the manifest files synced from config/crd/bases by the
// Makefile manifests target.
var embeddedCRDs = []string{
	"infrastructure.opendatahub.io_databaseclaims.yaml",
	"infrastructure.opendatahub.io_databaseproviders.yaml",
	"infrastructure.opendatahub.io_schemaclaims.yaml",
}

const sourceCRDDir = "../../../../config/crd/bases"

func TestEmbeddedManifestsMatchCRDBases(t *testing.T) {
	for _, file := range embeddedCRDs {
		t.Run(file, func(t *testing.T) {
			g := NewWithT(t)

			source, err := os.ReadFile(filepath.Join(sourceCRDDir, file))
			g.Expect(err).NotTo(HaveOccurred())
			embedded, err := manifestFS.ReadFile("manifests/" + file)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(embedded).To(Equal(source),
				"embedded operand manifests drifted from config/crd/bases; run `make manifests` to sync them")
		})
	}
}

func TestKustomizationListsExactlyTheOperandCRDs(t *testing.T) {
	g := NewWithT(t)

	raw, err := manifestFS.ReadFile("manifests/kustomization.yaml")
	g.Expect(err).NotTo(HaveOccurred())

	var kustomization struct {
		Resources []string `json:"resources"`
	}
	g.Expect(yaml.Unmarshal(raw, &kustomization)).To(Succeed())
	g.Expect(kustomization.Resources).To(ConsistOf(embeddedCRDs),
		"the operand kustomization must list exactly the synced CRDs")
}

func TestEmbeddedManifestsRender(t *testing.T) {
	g := NewWithT(t)

	fs, err := FS()
	g.Expect(err).NotTo(HaveOccurred())

	engine := kustomize.NewEngine(kustomize.WithEngineFS(fs))
	rendered, err := engine.Render(".")
	g.Expect(err).NotTo(HaveOccurred())

	crdGVK := schema.GroupVersionKind{
		Group:   "apiextensions.k8s.io",
		Version: "v1",
		Kind:    "CustomResourceDefinition",
	}
	names := make([]string, 0, len(rendered))
	for i := range rendered {
		g.Expect(rendered[i].GroupVersionKind()).To(Equal(crdGVK))
		names = append(names, rendered[i].GetName())
	}
	g.Expect(names).To(ConsistOf(
		infraApi.DatabaseProviderCRDName,
		infraApi.DatabaseClaimCRDName,
		infraApi.SchemaClaimCRDName,
	))
}

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

// Package operands embeds the module operand manifests for runtime
// installation. Nothing consumes FS() yet: a follow-up change for
// RHOAIENG-100934 renders and applies these CRDs, at which point this
// note goes away. Until then the sync and render tests keep the embedded
// copies honest. config/crd/bases remains the source of truth: the Makefile
// manifests target syncs the generated CRDs into the embedded directory,
// and the sync test fails the build when the copies drift.
package operands

import (
	"embed"

	fwfs "github.com/opendatahub-io/odh-platform-utilities/framework/render/kustomize/fs"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

//go:embed manifests
var manifestFS embed.FS

// FS returns a read-only kustomize filesystem rooted at the embedded
// manifests directory, whose root kustomization lists the operand resources.
func FS() (filesys.FileSystem, error) {
	return fwfs.NewFromIOFS(manifestFS, "manifests")
}

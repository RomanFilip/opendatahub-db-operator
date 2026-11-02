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

package chartgen

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/opendatahub-io/opendatahub-db-operator/pkg/config"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/resources/gvk"
)

const (
	defaultOutputDir   = "config/chart"
	defaultChartName   = "opendatahub-db-operator"
	defaultChartVer    = "0.1.0"
	crdsDirName        = "crds"
	templatesDirName   = "templates"
	chartYAMLFilename  = "Chart.yaml"
	helpersTplFilename = "_helpers.tpl"
	valuesYAMLFilename = "values.yaml"
	valuesSchemaFile   = "values.schema.json"
	coreAPIGroup       = "core"
)

// NewCommand returns the cobra command for the chartgen subcommand.
func NewCommand() *cobra.Command {
	var outputDir string
	var chartName string
	var chartVersion string

	cmd := &cobra.Command{
		Use:   "chartgen",
		Short: "Generate a Helm chart from kustomize YAML on stdin",
		Long: `Reads multi-document Kubernetes YAML from stdin (typically piped from
kustomize build) and generates a Helm chart with proper templating.

Example:
  kustomize build config/default | manager chartgen --output config/chart`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(os.Stdin, outputDir, chartName, chartVersion)
		},
	}

	cmd.Flags().StringVarP(&outputDir, "output", "o", defaultOutputDir, "Output directory for the chart")
	cmd.Flags().StringVar(&chartName, "name", defaultChartName, "Chart name")
	cmd.Flags().StringVar(&chartVersion, "version", defaultChartVer, "Chart version")

	return cmd
}

// buildChartContext assembles the operator identity chartgen's transforms
// rely on: which Deployment is the operator's, the pod labels and webhook
// port its template declares, its ConfigMap and ServiceAccount references,
// and whether that account is chart-managed.
func buildChartContext(
	operatorDeployment *unstructured.Unstructured,
	operatorConfigMapName string,
	operatorConfigMapStableName string,
	saName, saNamespace string,
	saManaged bool,
) chartContext {
	return chartContext{
		operatorDeployment: types.NamespacedName{
			Name:      operatorDeployment.GetName(),
			Namespace: operatorDeployment.GetNamespace(),
		},
		operatorPodLabels:   operatorPodTemplateLabels(operatorDeployment),
		operatorWebhookPort: operatorWebhookContainerPort(operatorDeployment),
		operatorConfigMap: types.NamespacedName{
			Name:      operatorConfigMapName,
			Namespace: operatorDeployment.GetNamespace(),
		},
		operatorConfigMapStableName: operatorConfigMapStableName,
		operatorServiceAccount: types.NamespacedName{
			Name:      saName,
			Namespace: saNamespace,
		},
		operatorServiceAccountManaged: saManaged,
	}
}

// operatorPodTemplateLabels returns the operator Deployment's pod-template
// labels, or nil when unreadable -- the identity a NetworkPolicy must select
// before chartgen templates its webhook ingress port.
func operatorPodTemplateLabels(deployment *unstructured.Unstructured) map[string]string {
	labels, found, err := unstructured.NestedStringMap(
		deployment.Object, "spec", "template", "metadata", "labels")
	if err != nil || !found {
		return nil
	}
	return labels
}

// operatorWebhookContainerPort returns the containerPort of the port named
// webhook-server on the manager container, or 0 when it has none. Only the
// manager container is consulted -- the same container the Deployment
// transform templates -- so a sidecar declaring its own webhook-server port
// cannot shift the value. The NetworkPolicy transform aligns its ingress
// port to this value, so the admitted port is derived from what the
// Deployment actually declares instead of a hardcoded default -- a
// kustomize-side port change cannot desync the policy from the listener.
func operatorWebhookContainerPort(deployment *unstructured.Unstructured) int64 {
	containers, found, err := unstructured.NestedSlice(
		deployment.Object, "spec", "template", "spec", "containers")
	if err != nil || !found {
		return 0
	}
	manager, ok := findManagerContainer(containers)
	if !ok {
		return 0
	}
	ports, found, err := unstructured.NestedSlice(manager, "ports")
	if err != nil || !found {
		return 0
	}
	for _, portItem := range ports {
		port, ok := portItem.(map[string]any)
		if !ok || port["name"] != webhookServerPortName {
			continue
		}
		switch value := port["containerPort"].(type) {
		case int64:
			return value
		case float64:
			return int64(value)
		}
	}
	return 0
}

//nolint:gocyclo // orchestrates decode, validation, render, and atomic chart publish
func run(
	reader io.Reader,
	outputDir string,
	chartName string,
	chartVersion string,
) error {
	resources, err := decodeResources(reader)
	if err != nil {
		return fmt.Errorf("decoding resources: %w", err)
	}

	// Reject empty input so a failed upstream build cannot publish a resource-free chart.
	if len(resources) == 0 {
		return fmt.Errorf("no resources in input -- refusing to overwrite %s with an empty chart", outputDir)
	}

	// Resolve "the operator Deployment" exactly once. Every other
	// per-Deployment fact (its ConfigMap, its ServiceAccount, its default
	// image/resources/replicas) is derived from this same object below, so
	// they can never disagree with each other -- or with which Deployment
	// gets the operator-specific rendering in transformResource -- the way
	// independent scans over resources could if more than one Deployment
	// happened to look like a candidate.
	operatorDeployment, err := findOperatorDeployment(resources)
	if err != nil {
		return fmt.Errorf("%w -- refusing to overwrite %s with an ambiguous chart", err, outputDir)
	}

	groups := groupByGVK(resources)

	values, err := ExtractDefaults(operatorDeployment, resources)
	if err != nil {
		return fmt.Errorf("extracting default values: %w", err)
	}

	operatorConfigMapName, err := OperatorConfigMapName(operatorDeployment)
	if err != nil {
		return fmt.Errorf("finding operator ConfigMap: %w", err)
	}
	// A ConfigMap volume is always resolved in the pod's own namespace, so
	// operatorDeployment's namespace (not some other resource's) is the
	// correct match here.
	configMapManaged := resourceExists(resources, gvk.ConfigMap, operatorConfigMapName, operatorDeployment.GetNamespace())
	if operatorConfigMapName != "" && !configMapManaged {
		// The manager container's configuration volume names a ConfigMap
		// this bundle doesn't itself create -- a pre-existing/externally
		// managed config, which config/manager/kustomization.yaml doesn't
		// currently do but nothing stops a future input from doing. There's
		// no template for it, so nothing downstream can safely treat this
		// name as "the" operator ConfigMap: stampConfigChecksumPlaceholder
		// must not reference a core_v1_configmap.yaml template chartgen
		// will never generate -- Helm's `include` would fail to find it at
		// render time. (ExtractDefaults, above, already only reads a
		// ConfigMap matching this exact name AND operatorDeployment's own
		// namespace, so it can't have picked up a wrong-namespace,
		// same-named one regardless of what this check finds.)
		operatorConfigMapName = ""
	}
	operatorConfigMapStableName := ""
	if operatorConfigMapName != "" {
		operatorConfigMapStableName = config.OperatorConfigMapName
	}
	if err := validateConfigMapStableNames(resources, types.NamespacedName{
		Name:      operatorConfigMapName,
		Namespace: operatorDeployment.GetNamespace(),
	}, operatorConfigMapStableName); err != nil {
		return err
	}

	saName, saNamespace, err := OperatorServiceAccountRef(operatorDeployment)
	if err != nil {
		return fmt.Errorf("finding operator ServiceAccount: %w", err)
	}
	// saManaged is deliberately not folded into saName/saNamespace by
	// resetting them to "" the way operatorConfigMapName is above: a
	// ServiceAccount's identity and its chart-ownership are different
	// questions downstream. config/rbac/kustomization.yaml documents
	// commenting out service_account.yaml to use an account that already
	// exists on the cluster -- in that case there's no chart-managed
	// identity to rename anything to, but the account (and any RoleBinding
	// subject naming it) still moves to wherever this chart installs, the
	// same as every other resource in the bundle. Losing the identity
	// entirely (the old behavior) meant that namespace tracking got
	// skipped too, not just the renaming. Namespace-qualified existence
	// check: a same-named ServiceAccount in some other namespace is a
	// different account, not evidence this one is chart-managed.
	saManaged := saName != "" && resourceExists(resources, gvk.ServiceAccount, saName, saNamespace)

	chartCtx := buildChartContext(
		operatorDeployment,
		operatorConfigMapName,
		operatorConfigMapStableName,
		saName,
		saNamespace,
		saManaged,
	)

	// Render every template into memory FIRST, before touching outputDir at
	// all. transformResource can still fail partway through this loop (an
	// unreadable ConfigMap data map, and so on) -- input-shape problems
	// findOperatorDeployment above doesn't catch. Discovering one only after
	// templatesDir has already been wiped would leave an empty or partial
	// chart in its place, which `helm upgrade` would then apply, removing
	// whatever the previous, real chart had installed. Rendering to a map
	// first means a failure here leaves outputDir completely untouched.
	renderedTemplates, renderedCRDs, err := renderResourceGroups(groups, chartCtx)
	if err != nil {
		return err
	}
	renderedTemplates[helpersTplFilename] = helpersTpl

	valuesYAML, err := MarshalValuesYAML(values)
	if err != nil {
		return fmt.Errorf("marshaling %s: %w", valuesYAMLFilename, err)
	}

	valuesSchema, err := MarshalValuesSchema()
	if err != nil {
		return fmt.Errorf("marshaling %s: %w", valuesSchemaFile, err)
	}

	// Everything above is pure computation; only from here does run() touch
	// disk, and only with content that's already been fully validated.

	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}

	// Write Chart.yaml (only if missing) -- additive, never destructive.
	chartFile := filepath.Join(outputDir, chartYAMLFilename)
	if _, err := os.Stat(chartFile); os.IsNotExist(err) {
		if err := writeChartYAML(chartFile, chartName, chartVersion); err != nil {
			return fmt.Errorf("writing %s: %w", chartYAMLFilename, err)
		}
	}

	// Stage everything else -- values.yaml, values.schema.json, and the
	// templates directory -- in a scratch area, then publish each one with
	// publishPath (see its doc comment for exactly what atomicity it does
	// and doesn't provide). Writing values.yaml/values.schema.json directly
	// with os.WriteFile, as an earlier version of this function did, is not
	// atomic either: a crash or full disk mid-write leaves a truncated file
	// in place over the previous, valid one.
	stagingDir, err := os.MkdirTemp(outputDir, ".chartgen-staging-*")
	if err != nil {
		return fmt.Errorf("creating staging directory: %w", err)
	}
	defer os.RemoveAll(stagingDir) //nolint:errcheck // best-effort cleanup; no-op after successful publishing below.

	stagingTemplatesDir := filepath.Join(stagingDir, templatesDirName)
	if err := os.MkdirAll(stagingTemplatesDir, 0o755); err != nil {
		return fmt.Errorf("creating staging templates directory: %w", err)
	}
	stagingCRDsDir := filepath.Join(stagingDir, crdsDirName)
	if err := os.MkdirAll(stagingCRDsDir, 0o755); err != nil {
		return fmt.Errorf("creating staging CRDs directory: %w", err)
	}
	for filename, content := range renderedTemplates {
		if err := os.WriteFile(filepath.Join(stagingTemplatesDir, filename), []byte(content), 0o644); err != nil {
			return fmt.Errorf("staging %s: %w", filename, err)
		}
	}
	for filename, content := range renderedCRDs {
		if err := os.WriteFile(filepath.Join(stagingCRDsDir, filename), []byte(content), 0o644); err != nil {
			return fmt.Errorf("staging CRD %s: %w", filename, err)
		}
	}

	if err := os.WriteFile(filepath.Join(stagingDir, valuesYAMLFilename), valuesYAML, 0o644); err != nil {
		return fmt.Errorf("staging %s: %w", valuesYAMLFilename, err)
	}
	if err := os.WriteFile(filepath.Join(stagingDir, valuesSchemaFile), valuesSchema, 0o644); err != nil {
		return fmt.Errorf("staging %s: %w", valuesSchemaFile, err)
	}

	if err := publishPath(stagingDir, outputDir, valuesYAMLFilename); err != nil {
		return err
	}
	if err := publishPath(stagingDir, outputDir, valuesSchemaFile); err != nil {
		return err
	}
	if err := publishPath(stagingDir, outputDir, templatesDirName); err != nil {
		return err
	}
	if err := publishPath(stagingDir, outputDir, crdsDirName); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Helm chart generated at %s\n", outputDir)

	return nil
}

func renderResourceGroups(
	groups map[schema.GroupVersionKind][]unstructured.Unstructured,
	chartCtx chartContext,
) (map[string]string, map[string]string, error) {
	renderedTemplates := make(map[string]string, len(groups))
	renderedCRDs := make(map[string]string)
	for resourceGVK, resources := range groups {
		filename := gvkToFilename(resourceGVK)
		content, err := renderGroup(resourceGVK, resources, chartCtx)
		if err != nil {
			return nil, nil, fmt.Errorf("rendering %s: %w", filename, err)
		}
		if isCRD(resourceGVK) {
			renderedCRDs[filename] = content
		} else {
			renderedTemplates[filename] = content
		}
	}

	return renderedTemplates, renderedCRDs, nil
}

// publishPath replaces one artifact through a backup rename and restores that backup after a failed publish.
// Each artifact is atomic; run() does not publish the complete chart as one transaction.
func publishPath(stagingDir, outputDir, name string) error {
	src := filepath.Join(stagingDir, name)
	dst := filepath.Join(outputDir, name)
	backup := dst + ".chartgen-previous"

	if _, dstErr := os.Lstat(dst); os.IsNotExist(dstErr) {
		if _, backupErr := os.Lstat(backup); backupErr == nil {
			if err := os.Rename(backup, dst); err != nil {
				return fmt.Errorf("recovering %s from a previous interrupted publish: %w", name, err)
			}
		}
	}

	if err := os.RemoveAll(backup); err != nil {
		return fmt.Errorf("clearing backup path for %s: %w", name, err)
	}

	hadPrevious := true
	if err := os.Rename(dst, backup); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("backing up existing %s: %w", name, err)
		}
		hadPrevious = false
	}

	if err := os.Rename(src, dst); err != nil {
		if hadPrevious {
			_ = os.Rename(backup, dst) // best-effort rollback -- restore what was there before.
		}
		return fmt.Errorf("publishing %s: %w", name, err)
	}

	if hadPrevious {
		if err := os.RemoveAll(backup); err != nil {
			return fmt.Errorf("removing backup of %s: %w", name, err)
		}
	}

	return nil
}

// decodeResources reads multi-document YAML from a reader and returns
// a slice of unstructured resources.
func decodeResources(reader io.Reader) ([]unstructured.Unstructured, error) {
	var resources []unstructured.Unstructured

	yr := utilyaml.NewYAMLReader(bufio.NewReader(reader))

	for {
		data, err := yr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading YAML document: %w", err)
		}

		data = []byte(strings.TrimSpace(string(data)))
		if len(data) == 0 {
			continue
		}

		var obj unstructured.Unstructured
		if err := yaml.Unmarshal(data, &obj.Object); err != nil {
			return nil, fmt.Errorf("unmarshaling resource: %w", err)
		}

		if obj.Object == nil {
			continue
		}

		resources = append(resources, obj)
	}

	return resources, nil
}

// resourceExists reports whether resources contains an object of the given
// kind with exactly this name AND namespace -- i.e. whether some identifier
// (a ConfigMap name read off a Deployment's volume, a ServiceAccount name
// read off its serviceAccountName) actually corresponds to something this
// bundle itself creates, as opposed to a reference to a pre-existing/
// externally managed resource that happens to share a name. Namespace is
// part of the check, not just name: a same-named ServiceAccount in some
// other namespace is a different account entirely, not evidence this one is
// chart-managed.
func resourceExists(
	resources []unstructured.Unstructured,
	target schema.GroupVersionKind,
	name, namespace string,
) bool {
	for _, r := range resources {
		if r.GroupVersionKind() == target && r.GetName() == name && r.GetNamespace() == namespace {
			return true
		}
	}

	return false
}

// Helm uses a content checksum for rollouts, so generated ConfigMaps need stable names.
func stripKustomizeConfigMapHash(name string) string {
	separator := strings.LastIndex(name, "-")
	if separator < 0 || len(name)-separator-1 != 10 {
		return name
	}

	for _, char := range name[separator+1:] {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') {
			return name
		}
	}

	return name[:separator]
}

func validateConfigMapStableNames(
	resources []unstructured.Unstructured,
	operatorConfigMap types.NamespacedName,
	operatorConfigMapStableName string,
) error {
	originalIdentitiesByEmittedName := make(map[string]types.NamespacedName)
	for i := range resources {
		resource := &resources[i]
		if resource.GroupVersionKind() != gvk.ConfigMap {
			continue
		}

		originalIdentity := types.NamespacedName{
			Name:      resource.GetName(),
			Namespace: resource.GetNamespace(),
		}
		emittedName := originalIdentity.Name
		if originalIdentity == operatorConfigMap {
			if operatorConfigMapStableName != "" {
				emittedName = operatorConfigMapStableName
			} else {
				emittedName = stripKustomizeConfigMapHash(emittedName)
			}
		}
		if previousIdentity, found := originalIdentitiesByEmittedName[emittedName]; found {
			return fmt.Errorf(
				"ConfigMap name collision: %s and %s both emit as %q in the chart namespace",
				previousIdentity,
				originalIdentity,
				emittedName,
			)
		}
		originalIdentitiesByEmittedName[emittedName] = originalIdentity
	}

	return nil
}

func isCRD(resourceGVK schema.GroupVersionKind) bool {
	return resourceGVK.Group == "apiextensions.k8s.io" && resourceGVK.Kind == "CustomResourceDefinition"
}

// groupByGVK groups resources by their GroupVersionKind, skipping Namespaces.
func groupByGVK(resources []unstructured.Unstructured) map[schema.GroupVersionKind][]unstructured.Unstructured {
	groups := make(map[schema.GroupVersionKind][]unstructured.Unstructured)

	for _, r := range resources {
		resourceGVK := r.GroupVersionKind()

		// Skip Namespace resources -- Helm manages namespace via --namespace
		if resourceGVK == gvk.Namespace {
			continue
		}

		groups[resourceGVK] = append(groups[resourceGVK], r)
	}

	return groups
}

// gvkToFilename converts a GVK to a template filename.
// Uses the full unambiguous format: <group>_<version>_<kind>.yaml
// Core API group (empty string) is rendered as "core".
func gvkToFilename(resourceGVK schema.GroupVersionKind) string {
	group := strings.ToLower(resourceGVK.Group)
	if group == "" {
		group = coreAPIGroup
	}

	return fmt.Sprintf("%s_%s_%s.yaml",
		group,
		strings.ToLower(resourceGVK.Version),
		strings.ToLower(resourceGVK.Kind),
	)
}

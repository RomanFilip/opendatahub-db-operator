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
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	"github.com/opendatahub-io/opendatahub-db-operator/pkg/resources/gvk"
)

const (
	yamlFieldNamespace = "namespace:"
	yamlFieldName      = "name:"
	yamlFieldData      = "data:"
	yamlFieldMetadata  = "metadata:"

	// metadataChildIndent is how many spaces sigs.k8s.io/yaml indents a
	// direct child of a top-level "metadata:" map (name, namespace, ...),
	// as opposed to something nested one level further inside, like a key
	// under metadata.labels/annotations.
	metadataChildIndent = 2

	tplReleaseNamespace   = "namespace: {{ .Release.Namespace }}"
	tplServiceAccountName = `{{ default (include "chart.fullname" .) .Values.serviceAccount.name }}`
	tplChartFullname      = `{{ include "chart.fullname" . }}`
	serviceCAGate         = `or .Values.openshift.serviceCA ` +
		`(.Capabilities.APIVersions.Has "security.openshift.io/v1")`
	serviceMonitorGate         = `{{- if and .Values.monitoring.serviceMonitor.enabled (` + serviceCAGate + `) }}`
	metricsReaderRoleName      = "odh-db-operator-metrics-reader"
	serviceCAAnnotation        = "service.beta.openshift.io/inject-cabundle"
	serviceCAServingAnnotation = "service.beta.openshift.io/serving-cert-secret-name"

	annotationCertManagerInjectCAFrom = "cert-manager.io/inject-ca-from"

	// managerContainerName identifies which container in the Deployment is
	// the operator itself. replaceImageField only ever templates this
	// container's image/imagePullPolicy -- any sidecar or init container
	// keeps whatever image it was given, verbatim.
	managerContainerName = "manager"

	// Sentinel values stamped onto the manager container's image/
	// imagePullPolicy fields before marshaling, so replaceImageField can
	// find and replace them by exact value rather than by guessing which
	// "image:"/"imagePullPolicy:" line in the whole Deployment belongs to
	// the operator's own container.
	imagePlaceholder          = "__CHARTGEN_MANAGER_IMAGE__"
	pullPolicyPlaceholder     = "__CHARTGEN_MANAGER_PULL_POLICY__"
	resourcesPlaceholder      = "__CHARTGEN_MANAGER_RESOURCES__"
	webhookServerPortName     = "webhook-server"
	webhookCertVolumeName     = "cert"
	webhookPortPlaceholder    = "__CHARTGEN_WEBHOOK_PORT__"
	webhookCertDirPlaceholder = "__CHARTGEN_WEBHOOK_CERT_DIR__"
	webhookCertDirDefault     = "/tmp/k8s-webhook-server/serving-certs"
	tplWebhookPort            = `{{ default 9443 (int (index .Values.config "controller.webhook.port")) }}`
	tplWebhookCertDir         = `{{ default "` + webhookCertDirDefault +
		`" (index .Values.config "controller.webhook.cert-dir") | quote }}`

	// Sentinel values stamped onto a RoleBinding/ClusterRoleBinding subject
	// that's been structurally confirmed to be the operator's own
	// ServiceAccount -- see stampOperatorSubjectPlaceholders.
	subjectNamePlaceholder      = "__CHARTGEN_OPERATOR_SUBJECT_NAME__"
	subjectNamespacePlaceholder = "__CHARTGEN_OPERATOR_SUBJECT_NAMESPACE__"

	// configChecksumAnnotationKey is the pod-template annotation that forces
	// a rollout whenever the operator's own ConfigMap content changes --
	// see stampConfigChecksumPlaceholder.
	configChecksumAnnotationKey = "checksum/config"
	configChecksumPlaceholder   = "__CHARTGEN_CONFIG_CHECKSUM__"

	// webhookServiceNamespacePlaceholder marks a webhook's
	// clientConfig.service.namespace for replaceWebhookNamespace -- see
	// stampWebhookServiceNamespacePlaceholders.
	webhookServiceNamespacePlaceholder = "__CHARTGEN_WEBHOOK_SERVICE_NAMESPACE__"

	// configMapDataSentinelKey stands in for a ConfigMap's "data" field when
	// it's absent or empty -- see ensureNonEmptyConfigMapData.
	configMapDataSentinelKey = "__CHARTGEN_EMPTY_DATA__"

	// imagePullSecretsSentinelName stands in for the pod spec's
	// imagePullSecrets field when it's absent or an empty list -- see
	// stampImagePullSecretsPlaceholder.
	imagePullSecretsSentinelName = "__CHARTGEN_EMPTY_IMAGE_PULL_SECRETS__"

	// replicasPlaceholder stands in for spec.replicas, stamped
	// unconditionally (it's always present in a Deployment either way, real
	// or defaulted) so replaceReplicas always has an exact value to find.
	replicasPlaceholder = "__CHARTGEN_REPLICAS__"

	// serviceAccountNamePlaceholder stands in for
	// spec.template.spec.serviceAccountName, stamped only when the account
	// is chart-managed -- see replaceServiceAccountName.
	serviceAccountNamePlaceholder = "__CHARTGEN_SERVICE_ACCOUNT_NAME__"
)

// chartContext carries identity facts about the operator's own resources --
// established once, structurally, from the full input set -- that transforms
// need to tell "this operator's own X" apart from some other resource of the
// same kind that the input might also contain.
type chartContext struct {
	// operatorDeployment is the identity of the single Deployment resolved
	// by findOperatorDeployment -- the one transformResource applies
	// operator-specific rendering to. Any other Deployment in the input
	// (there shouldn't be one today, but nothing enforces that) is left
	// alone via transformGeneric instead.
	operatorDeployment types.NamespacedName
	// operatorPodLabels are the operator Deployment's pod-template labels:
	// the identity a NetworkPolicy must select before chartgen templates
	// its webhook ingress port, so an unrelated policy in the input keeps
	// its literal ports.
	operatorPodLabels map[string]string
	// operatorWebhookPort is the webhook port the operator Deployment
	// actually declares; the NetworkPolicy transform templates the ingress
	// rule carrying this value, so the match derives from the listener
	// rather than a hardcoded default.
	operatorWebhookPort         int64
	operatorConfigMap           types.NamespacedName
	operatorConfigMapStableName string
	operatorServiceAccount      types.NamespacedName
	// operatorServiceAccountManaged is deliberately separate from whether
	// operatorServiceAccount.Name is set: the identity is always populated
	// whenever the Deployment names a serviceAccountName at all (managed or
	// not), because the RoleBinding subject namespace tracking below needs
	// to recognize the operator's own account either way. Only *renaming*
	// that account to a Values-driven fullname is conditional on it being
	// chart-managed.
	operatorServiceAccountManaged bool
}

// renderGroup renders a group of resources with the same GVK into a single
// Helm template file, applying kind-specific transformations.
func renderGroup(
	resourceGVK schema.GroupVersionKind,
	resources []unstructured.Unstructured,
	chartCtx chartContext,
) (string, error) {
	parts := make([]string, 0, len(resources))

	for i := range resources {
		transformed, err := transformResource(resourceGVK, &resources[i], chartCtx)
		if err != nil {
			return "", fmt.Errorf("transforming %s/%s: %w", resourceGVK.Kind, resources[i].GetName(), err)
		}

		parts = append(parts, transformed)
	}

	content := strings.Join(parts, "\n---\n")
	if resourceGVK == gvk.SecurityContextConstraints {
		// Kind and vanilla Kubernetes lack this API; skip at install time.
		return wrapOpenShiftAPIGuard(content), nil
	}
	return content, nil
}

// wrapOpenShiftAPIGuard renders the content when either the OpenShift SCC API
// is discoverable (standalone helm install/upgrade against a live cluster) or
// Values.openshift.securityContextConstraints is true.
//
// The ODH platform operator renders charts offline via renderer-helm with
// nil Capabilities (DefaultCapabilities only), so Capabilities alone would
// skip the SCC on OpenShift. Module handlers set the Values flag for that path.
func wrapOpenShiftAPIGuard(content string) string {
	const (
		begin = "{{- if or .Values.openshift.securityContextConstraints" +
			" (.Capabilities.APIVersions.Has \"security.openshift.io/v1\") }}\n"
		end = "\n{{- end }}\n"
	)
	return begin + content + end
}

var stripLabelKeys = []string{
	"app.kubernetes.io/managed-by",
}

func stripLabels(obj *unstructured.Unstructured) {
	labels := obj.GetLabels()
	if len(labels) == 0 {
		return
	}

	for _, key := range stripLabelKeys {
		delete(labels, key)
	}

	if len(labels) == 0 {
		obj.SetLabels(nil)
	} else {
		obj.SetLabels(labels)
	}
}

// transformResource applies Helm template transformations to a resource
// based on its kind.
func transformResource(
	resourceGVK schema.GroupVersionKind,
	obj *unstructured.Unstructured,
	chartCtx chartContext,
) (string, error) {
	stripLabels(obj)

	switch resourceGVK {
	case gvk.Deployment:
		isOperatorDeployment := obj.GetName() == chartCtx.operatorDeployment.Name &&
			obj.GetNamespace() == chartCtx.operatorDeployment.Namespace
		if !isOperatorDeployment {
			// Some other Deployment in the input -- not the one
			// findOperatorDeployment identified as the operator itself.
			// Templating it the same way (its image, resources, replicas,
			// and ServiceAccount all forced to match the real operator's)
			// would silently change an unrelated workload's identity.
			return transformGeneric(obj)
		}
		if chartCtx.operatorConfigMap.Name != "" && chartCtx.operatorConfigMapStableName != "" {
			if err := rewriteOperatorConfigMapName(
				obj,
				chartCtx.operatorConfigMap.Name,
				chartCtx.operatorConfigMapStableName,
			); err != nil {
				return "", fmt.Errorf("deployment %s: rewriting ConfigMap name: %w", obj.GetName(), err)
			}
		}

		return transformDeployment(obj, chartCtx.operatorConfigMap.Name != "", chartCtx.operatorServiceAccountManaged)
	case gvk.ServiceAccount:
		isOperatorSA := obj.GetName() == chartCtx.operatorServiceAccount.Name &&
			obj.GetNamespace() == chartCtx.operatorServiceAccount.Namespace
		if isOperatorSA {
			return transformServiceAccount(obj)
		}
		// Not the ServiceAccount the operator's own Deployment runs as --
		// leave its name alone rather than renaming it to the operator's
		// identity too, which would collide with an auxiliary account any
		// other resource in the input still references by its real name.
		return transformGeneric(obj)
	case gvk.ConfigMap:
		isOperatorConfigMap := obj.GetName() == chartCtx.operatorConfigMap.Name &&
			obj.GetNamespace() == chartCtx.operatorConfigMap.Namespace
		if isOperatorConfigMap {
			if chartCtx.operatorConfigMapStableName != "" {
				obj.SetName(chartCtx.operatorConfigMapStableName)
			}

			return transformConfigMap(obj)
		}
		// Not the ConfigMap mounted into the operator's own container --
		// leave its data alone rather than overwriting it with
		// .Values.platform.*/.Values.config templating meant for a
		// different ConfigMap entirely. Namespace-qualified: a same-named
		// ConfigMap in some other namespace is a different object.
		if hasAnnotation(obj, serviceCAAnnotation) {
			return transformServiceCAConfigMap(obj)
		}
		return transformGeneric(obj)
	case gvk.ClusterRole:
		return transformClusterRole(obj)
	case gvk.ClusterRoleBinding:
		return transformClusterRoleBinding(obj, chartCtx.operatorServiceAccount, chartCtx.operatorServiceAccountManaged)
	case gvk.RoleBinding:
		return transformRoleBinding(obj, chartCtx.operatorServiceAccount, chartCtx.operatorServiceAccountManaged)
	case gvk.MutatingWebhookConfiguration, gvk.ValidatingWebhookConfiguration:
		return transformWebhook(obj)
	case gvk.CertManagerCertificate:
		return transformCertificate(obj)
	case gvk.ServiceMonitor:
		return transformServiceMonitor(obj)
	case gvk.Service:
		return transformService(obj)
	case gvk.NetworkPolicy:
		return transformNetworkPolicy(obj, chartCtx.operatorPodLabels, chartCtx.operatorWebhookPort)
	default:
		return transformGeneric(obj)
	}
}

// Kustomize hashes generated ConfigMaps; use a stable name because
// checksum/config triggers rollouts when their content changes.
func rewriteOperatorConfigMapName(
	obj *unstructured.Unstructured,
	originalName string,
	stableName string,
) error {
	volumes, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "volumes")
	if err != nil || !found {
		return err
	}

	for _, item := range volumes {
		volume, ok := item.(map[string]any)
		if !ok {
			continue
		}
		configMap, ok := volume["configMap"].(map[string]any)
		if !ok {
			continue
		}
		if name, _ := configMap["name"].(string); name == originalName {
			configMap["name"] = stableName
		}
	}

	return unstructured.SetNestedSlice(obj.Object, volumes, "spec", "template", "spec", "volumes")
}

// transformDeployment injects Helm value references for the manager image,
// resources, webhook settings, replicas, imagePullSecrets, and serviceAccountName.
func transformDeployment(
	obj *unstructured.Unstructured,
	hasOperatorConfigMap bool,
	hasChartManagedServiceAccount bool,
) (string, error) {
	if err := stampManagerContainerPlaceholders(obj); err != nil {
		return "", fmt.Errorf("deployment %s: %w", obj.GetName(), err)
	}
	if err := stampWebhookPlaceholders(obj); err != nil {
		return "", fmt.Errorf("deployment %s: %w", obj.GetName(), err)
	}

	if hasOperatorConfigMap {
		if err := stampConfigChecksumPlaceholder(obj); err != nil {
			return "", fmt.Errorf("deployment %s: %w", obj.GetName(), err)
		}
	}

	if err := stampImagePullSecretsPlaceholder(obj); err != nil {
		return "", fmt.Errorf("deployment %s: %w", obj.GetName(), err)
	}

	// A bare prefix scan for "replicas:" would also match embedded YAML
	// inside an annotation value, and would never fire at all when
	// spec.replicas is simply omitted (a valid Deployment can leave it
	// unset, defaulting to 1) -- silently making .Values.operator.replicas
	// have no effect. Stamping the exact field structurally sidesteps both.
	if err := unstructured.SetNestedField(obj.Object, replicasPlaceholder, "spec", "replicas"); err != nil {
		return "", fmt.Errorf("deployment %s: stamping replicas: %w", obj.GetName(), err)
	}

	// Only stamp when the ServiceAccount is chart-managed (see the
	// hasChartManagedServiceAccount comment further down) -- otherwise
	// leave spec.template.spec.serviceAccountName completely untouched, not
	// just unmatched, so its literal, real value survives verbatim.
	if hasChartManagedServiceAccount {
		saPath := []string{"spec", "template", "spec", "serviceAccountName"}
		if err := unstructured.SetNestedField(obj.Object, serviceAccountNamePlaceholder, saPath...); err != nil {
			return "", fmt.Errorf("deployment %s: stamping serviceAccountName: %w", obj.GetName(), err)
		}
	}

	raw, err := marshalResource(obj)
	if err != nil {
		return "", err
	}

	raw = replaceNamespace(raw)

	raw = replaceImageField(raw)

	raw = replaceReplicas(raw)

	raw = replaceResourcesField(raw)
	raw = strings.ReplaceAll(raw, webhookPortPlaceholder, tplWebhookPort)
	raw = strings.ReplaceAll(raw, webhookCertDirPlaceholder, tplWebhookCertDir)

	// Replace serviceAccountName. hasChartManagedServiceAccount gates
	// whether the field was stamped with a sentinel above at all -- when
	// it's an externally-managed account (config/rbac/kustomization.yaml
	// documents this as an option), nothing was stamped, so this is a no-op
	// and the real name survives untouched; pointing an external reference
	// at a Values-driven, chart-managed identity nothing ever creates would
	// leave the pod unable to start.
	raw = replaceServiceAccountName(raw)

	raw = addImagePullSecrets(raw)

	if hasOperatorConfigMap {
		raw = replaceConfigChecksumPlaceholder(raw)
	}

	return raw, nil
}

// stampConfigChecksumPlaceholder adds a sentinel pod-template annotation
// that replaceConfigChecksumPlaceholder later expands into a Helm checksum
// expression over the operator's own rendered ConfigMap template. Without
// it, a Helm upgrade that only changes .Values.platform/.Values.config
// changes the ConfigMap's data but nothing in the Deployment's pod
// template, so already-running pods -- which read their configuration once
// at startup -- keep running with the old configuration indefinitely; only
// a later, unrelated rollout would pick up the change.
func stampConfigChecksumPlaceholder(obj *unstructured.Unstructured) error {
	annotations, found, err := unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "annotations")
	if err != nil {
		return fmt.Errorf("reading spec.template.metadata.annotations: %w", err)
	}
	if !found {
		annotations = map[string]string{}
	}

	annotations[configChecksumAnnotationKey] = configChecksumPlaceholder

	return unstructured.SetNestedStringMap(obj.Object, annotations, "spec", "template", "metadata", "annotations")
}

// replaceConfigChecksumPlaceholder swaps the sentinel stamped above for a
// Helm expression that hashes the operator's own ConfigMap template's
// rendered content. gvkToFilename(gvk.ConfigMap) is the exact filename
// chartgen itself writes that template under, so this always points at the
// right file without needing to know the ConfigMap's own (possibly
// kustomize-hash-suffixed) name.
func replaceConfigChecksumPlaceholder(raw string) string {
	expr := `{{ include (print $.Template.BasePath "/` + gvkToFilename(gvk.ConfigMap) + `") . | sha256sum }}`

	return strings.ReplaceAll(raw, configChecksumPlaceholder, expr)
}

// findManagerContainer returns the container named managerContainerName from
// a spec.template.spec.containers slice, if present. This is the single
// place that decides "which container is the operator" -- ExtractDefaults
// (values.go) and stampManagerContainerPlaceholders both use it, so the
// values.yaml defaults and the rendered chart template can never disagree
// about which container they each mean by "the operator".
func findManagerContainer(containers []any) (map[string]any, bool) {
	for _, c := range containers {
		container, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := container["name"].(string); name == managerContainerName {
			return container, true
		}
	}

	return nil, false
}

// stampManagerContainerPlaceholders finds the container named
// managerContainerName in obj's pod spec and overwrites its image and
// imagePullPolicy fields with sentinel placeholder strings, mutating obj in
// place. This identifies the operator's own container structurally --
// before marshaling to text -- so the later text-based replaceImageField
// pass can replace those two fields unambiguously by their exact sentinel
// value instead of matching any "image:"/"imagePullPolicy:" line anywhere
// in the Deployment, which would also rewrite (or, depending on field
// order, silently miss) a sidecar or init container's own image.
func stampManagerContainerPlaceholders(obj *unstructured.Unstructured) error {
	containers, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	if err != nil {
		return fmt.Errorf("reading spec.template.spec.containers: %w", err)
	}
	if !found {
		return fmt.Errorf("no spec.template.spec.containers")
	}

	container, ok := findManagerContainer(containers)
	if !ok {
		return fmt.Errorf("no container named %q", managerContainerName)
	}

	container["image"] = imagePlaceholder
	container["imagePullPolicy"] = pullPolicyPlaceholder
	// A single string sentinel, not a nested placeholder map, so it
	// marshals as one line regardless of whether the original "resources"
	// was a populated map or an empty {} -- replaceResourcesField expands
	// that one line into the templated block either way, closing the gap
	// where an empty resources: {} on the manager container used to be
	// skipped entirely (nothing to match) and never got templated.
	container["resources"] = resourcesPlaceholder

	return unstructured.SetNestedSlice(obj.Object, containers, "spec", "template", "spec", "containers")
}

func stampWebhookPlaceholders(obj *unstructured.Unstructured) error {
	containers, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	if err != nil {
		return fmt.Errorf("reading spec.template.spec.containers: %w", err)
	}
	if !found {
		return nil
	}

	container, ok := findManagerContainer(containers)
	if !ok {
		return fmt.Errorf("no container named %q", managerContainerName)
	}

	ports, found, err := unstructured.NestedSlice(container, "ports")
	if err != nil {
		return fmt.Errorf("reading manager container ports: %w", err)
	}
	if found {
		for _, item := range ports {
			port, ok := item.(map[string]any)
			if !ok || port["name"] != webhookServerPortName {
				continue
			}
			if _, found := port["containerPort"]; !found {
				return fmt.Errorf("manager port %q has no containerPort", webhookServerPortName)
			}
			port["containerPort"] = webhookPortPlaceholder
			break
		}
		if err := unstructured.SetNestedSlice(container, ports, "ports"); err != nil {
			return fmt.Errorf("stamping manager webhook port: %w", err)
		}
	}

	volumeMounts, found, err := unstructured.NestedSlice(container, "volumeMounts")
	if err != nil {
		return fmt.Errorf("reading manager container volume mounts: %w", err)
	}
	if found {
		for _, item := range volumeMounts {
			volumeMount, ok := item.(map[string]any)
			if !ok || volumeMount["name"] != webhookCertVolumeName {
				continue
			}
			if _, found := volumeMount["mountPath"]; !found {
				return fmt.Errorf("manager volume mount %q has no mountPath", webhookCertVolumeName)
			}
			volumeMount["mountPath"] = webhookCertDirPlaceholder
			break
		}
		if err := unstructured.SetNestedSlice(container, volumeMounts, "volumeMounts"); err != nil {
			return fmt.Errorf("stamping manager webhook certificate directory: %w", err)
		}
	}

	return unstructured.SetNestedSlice(obj.Object, containers, "spec", "template", "spec", "containers")
}

// transformServiceAccount injects Helm value references for the name and
// annotations, and always creates the ServiceAccount.
func transformServiceAccount(obj *unstructured.Unstructured) (string, error) {
	staticAnnotations := obj.GetAnnotations()

	raw, err := marshalResource(obj)
	if err != nil {
		return "", err
	}

	raw = replaceNamespace(raw)
	raw = replaceServiceAccountMetadata(raw, obj.GetName(), staticAnnotations)

	return raw, nil
}

// transformConfigMap injects Helm value merging for explicit platform keys
// and arbitrary .Values.config entries.
func transformConfigMap(obj *unstructured.Unstructured) (string, error) {
	ensureNonEmptyConfigMapData(obj)

	raw, err := marshalResource(obj)
	if err != nil {
		return "", err
	}

	raw = replaceNamespace(raw)
	raw = injectConfigMapValues(raw)

	return raw, nil
}

// ensureNonEmptyConfigMapData guarantees obj's "data" field marshals as a
// multi-line block ("data:\n  key: value") rather than either being absent
// entirely or collapsing to the one-line "data: {}" form empty maps
// serialize as. injectConfigMapValues finds and replaces the ConfigMap's
// "data:" block by scanning marshaled text for a line reading exactly
// "data:" -- an absent field produces no such line at all, and "data: {}"
// isn't textually equal to "data:" either, so a ConfigMap with no data or
// an explicitly empty one used to skip the replacement entirely, silently
// leaving Values.platform/Values.config with no effect on the rendered
// chart. Stamping a single throwaway key when there's nothing else there
// gives injectConfigMapValues's existing (and otherwise-unmodified)
// skip-and-replace logic something real to find, exactly as if the source
// had one actual key.
func ensureNonEmptyConfigMapData(obj *unstructured.Unstructured) {
	data, found, _ := unstructured.NestedMap(obj.Object, "data")
	if found && len(data) > 0 {
		return
	}

	_ = unstructured.SetNestedMap(obj.Object, map[string]any{configMapDataSentinelKey: ""}, "data")
}

// transformRoleBinding replaces the operator's own ServiceAccount subject
// (identified structurally by the full name+namespace the Deployment
// actually runs as) with the Helm value reference. Any other subject -- a
// same-named ServiceAccount in a different namespace, a different
// ServiceAccount, or a User/Group -- is left exactly as it was: a binding is
// not necessarily only about this operator's own account, and matching by
// name alone would let a same-named account elsewhere silently receive (or
// be renamed into) this operator's identity.
//
// renameManaged is intentionally separate from whether operatorServiceAccount
// is even known: the subject's *namespace* tracks the Helm release namespace
// whenever this really is the operator's own account, whether or not that
// account is chart-managed -- every resource in this bundle, external
// account or not, still moves to wherever the chart installs. Only the
// subject's *name* is rewritten to a Values-driven fullname, and only when
// the account is chart-managed; an externally-managed account keeps its
// real, literal name (see the "config/rbac/kustomization.yaml documents an
// existing account" scenario transformDeployment's own comment covers).
func transformRoleBinding(
	obj *unstructured.Unstructured,
	operatorServiceAccount types.NamespacedName,
	renameManaged bool,
) (string, error) {
	roleRef, found, err := unstructured.NestedMap(obj.Object, "roleRef")
	if err != nil {
		return "", fmt.Errorf("reading roleRef: %w", err)
	}
	if found {
		roleName, _ := roleRef["name"].(string)
		roleKind, _ := roleRef["kind"].(string)
		if roleKind == "ClusterRole" && strings.HasPrefix(roleName, "odh-db-operator-") {
			roleRef["name"] = releaseScopedName(roleName)
			if err := unstructured.SetNestedMap(obj.Object, roleRef, "roleRef"); err != nil {
				return "", err
			}
		}
	}

	if err := stampOperatorSubjectPlaceholders(obj, operatorServiceAccount, renameManaged); err != nil {
		return "", fmt.Errorf("role binding %s: %w", obj.GetName(), err)
	}

	raw, err := marshalResource(obj)
	if err != nil {
		return "", err
	}

	raw = replaceNamespace(raw)
	raw = replaceSubjectPlaceholders(raw)

	return raw, nil
}

func releaseScopedName(name string) string {
	suffix := strings.TrimPrefix(name, "odh-db-operator-")
	return fmt.Sprintf(
		`{{ include "chart.clusterResourceName" (dict "root" . "suffix" %q) }}`,
		suffix,
	)
}

func transformClusterRole(obj *unstructured.Unstructured) (string, error) {
	name := obj.GetName()
	if strings.HasPrefix(name, "odh-db-operator-") {
		obj.SetName(releaseScopedName(name))
	}
	return transformGeneric(obj)
}

func transformClusterRoleBinding(
	obj *unstructured.Unstructured,
	operatorServiceAccount types.NamespacedName,
	renameManaged bool,
) (string, error) {
	roleRef, found, err := unstructured.NestedMap(obj.Object, "roleRef")
	if err != nil {
		return "", fmt.Errorf("reading roleRef: %w", err)
	}
	if !found {
		return "", fmt.Errorf("roleRef is missing")
	}
	roleName, _ := roleRef["name"].(string)
	metricsReaderBinding := strings.HasSuffix(roleName, "-metrics-reader")
	if roleName != "" {
		roleRef["name"] = releaseScopedName(roleName)
		if err := unstructured.SetNestedMap(obj.Object, roleRef, "roleRef"); err != nil {
			return "", err
		}
	}

	if err := stampOperatorSubjectPlaceholders(obj, operatorServiceAccount, renameManaged); err != nil {
		return "", fmt.Errorf("role binding %s: %w", obj.GetName(), err)
	}

	if metricsReaderBinding {
		if err := configureCollectorSubject(obj); err != nil {
			return "", err
		}
	}

	obj.SetName(releaseScopedName(obj.GetName()))
	raw, err := marshalResource(obj)
	if err != nil {
		return "", err
	}

	raw = replaceNamespace(raw)
	raw = replaceSubjectPlaceholders(raw)
	return raw, nil
}

func configureCollectorSubject(obj *unstructured.Unstructured) error {
	subjects, found, err := unstructured.NestedSlice(obj.Object, "subjects")
	if err != nil {
		return fmt.Errorf("reading subjects: %w", err)
	}
	if !found {
		return fmt.Errorf("subjects are missing")
	}

	for _, item := range subjects {
		subject, ok := item.(map[string]any)
		if !ok || subject["kind"] != "ServiceAccount" {
			continue
		}
		subject["name"] = "{{ .Values.monitoring.serviceMonitor.collector.serviceAccount.name }}"
		subject["namespace"] = "{{ .Values.monitoring.serviceMonitor.collector.serviceAccount.namespace }}"
	}

	return unstructured.SetNestedSlice(obj.Object, subjects, "subjects")
}

// stampOperatorSubjectPlaceholders overwrites the namespace (always, when
// this subject really is the operator's own account) and the name (only
// when renameManaged) of any subject in obj's "subjects" list whose kind is
// ServiceAccount and whose full name+namespace identity matches the
// operator's own, with sentinel placeholders -- mirroring
// stampManagerContainerPlaceholders' approach for the Deployment's image
// field, for the same reason: replacing an exact sentinel value afterward is
// unambiguous, where scanning rendered text for "name:"/"namespace:" lines
// inside a "subjects:" block is not. A RoleBinding subject with no
// "namespace" field is resolved to the binding's own namespace (the
// ServiceAccount kind's real default) before comparing -- a
// ClusterRoleBinding has no such implicit namespace, so an omitted
// namespace there can never resolve to a known identity and never matches.
func stampOperatorSubjectPlaceholders(
	obj *unstructured.Unstructured,
	operatorServiceAccount types.NamespacedName,
	renameManaged bool,
) error {
	if operatorServiceAccount.Name == "" {
		return nil
	}

	subjects, found, err := unstructured.NestedSlice(obj.Object, "subjects")
	if err != nil {
		return fmt.Errorf("reading subjects: %w", err)
	}
	if !found {
		return nil
	}

	bindingNamespace := obj.GetNamespace()

	for _, s := range subjects {
		subject, ok := s.(map[string]any)
		if !ok {
			continue
		}

		kind, _ := subject["kind"].(string)
		name, _ := subject["name"].(string)
		if kind != "ServiceAccount" || name != operatorServiceAccount.Name {
			continue
		}

		subjectNamespace, hasNamespace := subject["namespace"].(string)
		if subjectNamespace == "" {
			subjectNamespace = bindingNamespace
		}
		if subjectNamespace != operatorServiceAccount.Namespace {
			continue
		}

		if hasNamespace {
			subject["namespace"] = subjectNamespacePlaceholder
		}
		if renameManaged {
			subject["name"] = subjectNamePlaceholder
		}
	}

	return unstructured.SetNestedSlice(obj.Object, subjects, "subjects")
}

// replaceSubjectPlaceholders replaces the sentinel values
// stampOperatorSubjectPlaceholders wrote with their Helm template
// equivalents.
func replaceSubjectPlaceholders(raw string) string {
	raw = strings.ReplaceAll(raw, subjectNamePlaceholder, tplServiceAccountName)
	raw = strings.ReplaceAll(raw, subjectNamespacePlaceholder, "{{ .Release.Namespace }}")

	return raw
}

// transformWebhook replaces webhook service namespace references and
// cert-manager annotation namespace.
func transformWebhook(obj *unstructured.Unstructured) (string, error) {
	if err := stampWebhookServiceNamespacePlaceholders(obj); err != nil {
		return "", fmt.Errorf("webhook %s: %w", obj.GetName(), err)
	}

	raw, err := marshalResource(obj)
	if err != nil {
		return "", err
	}

	raw = replaceWebhookNamespace(raw)

	return raw, nil
}

// stampWebhookServiceNamespacePlaceholders overwrites
// webhooks[].clientConfig.service.namespace with a sentinel for every
// webhook entry that has one. replaceNamespace only ever touches a
// resource's own top-level metadata.namespace (by design, since anything
// looser corrupts unrelated content -- see replaceNamespace's own doc
// comment), so a webhook's *target* Service namespace, nested several
// levels deeper, needs this separate, still-structural mechanism to be
// rewritten at all.
func stampWebhookServiceNamespacePlaceholders(obj *unstructured.Unstructured) error {
	webhooks, found, err := unstructured.NestedSlice(obj.Object, "webhooks")
	if err != nil {
		return fmt.Errorf("reading webhooks: %w", err)
	}
	if !found {
		return nil
	}

	for _, w := range webhooks {
		webhook, ok := w.(map[string]any)
		if !ok {
			continue
		}
		clientConfig, ok := webhook["clientConfig"].(map[string]any)
		if !ok {
			continue
		}
		service, ok := clientConfig["service"].(map[string]any)
		if !ok {
			continue
		}
		if _, hasNamespace := service["namespace"]; hasNamespace {
			service["namespace"] = webhookServiceNamespacePlaceholder
		}
	}

	return unstructured.SetNestedSlice(obj.Object, webhooks, "webhooks")
}

// transformCertificate replaces hardcoded namespace references in
// cert-manager Certificate dnsNames with the Helm release namespace.
func transformCertificate(obj *unstructured.Unstructured) (string, error) {
	raw, err := marshalResource(obj)
	if err != nil {
		return "", err
	}

	raw = replaceNamespace(raw)
	raw = replaceCertificateDNSNames(raw)

	return raw, nil
}

// replaceCertificateDNSNames replaces hardcoded namespace segments in
// Certificate dnsNames entries with the Helm release namespace template.
// dnsNames follow the pattern: <service>.<namespace>.svc[.cluster.local]
func replaceCertificateDNSNames(raw string) string {
	lines := strings.Split(raw, "\n")
	inDNSNames := false

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		if trimmed == "dnsNames:" {
			inDNSNames = true

			continue
		}

		// End dnsNames section when we hit a non-list-item line.
		if inDNSNames && !strings.HasPrefix(trimmed, "-") && trimmed != "" {
			inDNSNames = false
		}

		if inDNSNames && strings.HasPrefix(trimmed, "- ") && strings.Contains(trimmed, ".svc") {
			// Extract the service name (first segment before the first dot).
			entry := strings.TrimPrefix(trimmed, "- ")
			parts := strings.SplitN(entry, ".", 3)
			if len(parts) >= 3 {
				indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
				suffix := parts[2] // "svc" or "svc.cluster.local"
				lines[i] = indent + "- " + parts[0] + ".{{ .Release.Namespace }}." + suffix
			}
		}
	}

	return strings.Join(lines, "\n")
}

// transformGeneric replaces the namespace for namespaced resources and
// passes cluster-scoped resources through as-is.
func transformGeneric(obj *unstructured.Unstructured) (string, error) {
	raw, err := marshalResource(obj)
	if err != nil {
		return "", err
	}

	if obj.GetNamespace() != "" {
		raw = replaceNamespace(raw)
	}

	return raw, nil
}

func transformServiceMonitor(obj *unstructured.Unstructured) (string, error) {
	raw, err := transformGeneric(obj)
	if err != nil {
		return "", err
	}

	raw = replaceServiceMonitorServerName(raw)

	return serviceMonitorGate + "\n" + raw + "\n{{- end }}", nil
}

func hasAnnotation(obj *unstructured.Unstructured, key string) bool {
	_, found := obj.GetAnnotations()[key]
	return found
}

func transformServiceCAConfigMap(obj *unstructured.Unstructured) (string, error) {
	raw, err := transformGeneric(obj)
	if err != nil {
		return "", err
	}

	return "{{- if " + serviceCAGate + " }}\n" + raw + "\n{{- end }}", nil
}

func transformService(obj *unstructured.Unstructured) (string, error) {
	raw, err := transformGeneric(obj)
	if err != nil {
		return "", err
	}
	if !hasAnnotation(obj, serviceCAServingAnnotation) {
		return raw, nil
	}

	return gateServiceAnnotation(raw), nil
}

func gateServiceAnnotation(raw string) string {
	lines := strings.Split(raw, "\n")
	for index, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), serviceCAServingAnnotation+":") {
			continue
		}

		indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
		gated := []string{
			indent + "{{- if " + serviceCAGate + " }}",
			line,
			indent + "{{- end }}",
		}
		lines = append(lines[:index], append(gated, lines[index+1:]...)...)
		return strings.Join(lines, "\n")
	}
	return raw
}

// replaceServiceMonitorServerName replaces the namespace segment in
// tlsConfig.serverName with the Helm release namespace template.
// serverName follows the pattern: <service>.<namespace>.svc[.cluster.local]
func replaceServiceMonitorServerName(raw string) string {
	lines := strings.Split(raw, "\n")

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		if !strings.HasPrefix(trimmed, "serverName: ") || !strings.Contains(trimmed, ".svc") {
			continue
		}

		value := strings.TrimPrefix(trimmed, "serverName: ")
		parts := strings.SplitN(value, ".", 3)
		if len(parts) < 3 {
			continue
		}

		indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
		lines[i] = indent + "serverName: " + parts[0] + ".{{ .Release.Namespace }}." + parts[2]
	}

	return strings.Join(lines, "\n")
}

// transformNetworkPolicy templates the webhook ingress port of the
// operator's own NetworkPolicy from the same chart value the Deployment
// listens on (controller.webhook.port). The rule to template is found by the
// port the operator Deployment actually declares, so a kustomize-side port
// change flows through instead of silently desyncing the policy from the
// listener, which would leave the API server's webhook calls denied by this
// very policy. A policy that does not select the operator pod keeps its
// literal ports.
func transformNetworkPolicy(
	obj *unstructured.Unstructured,
	operatorPodLabels map[string]string,
	operatorWebhookPort int64,
) (string, error) {
	if !policySelectsOperator(obj, operatorPodLabels) || operatorWebhookPort <= 0 {
		return transformGeneric(obj)
	}

	ingress, found, err := unstructured.NestedSlice(obj.Object, "spec", "ingress")
	if err != nil {
		return "", fmt.Errorf("networkpolicy %s: reading ingress rules: %w", obj.GetName(), err)
	}
	if !found {
		return transformGeneric(obj)
	}

	stamped := false
	for _, item := range ingress {
		rule, ok := item.(map[string]any)
		if !ok {
			continue
		}
		ports, found, err := unstructured.NestedSlice(rule, "ports")
		if err != nil || !found {
			continue
		}
		for _, portItem := range ports {
			port, ok := portItem.(map[string]any)
			if !ok {
				continue
			}
			switch portValue := port["port"].(type) {
			case int64:
				if portValue == operatorWebhookPort {
					port["port"] = webhookPortPlaceholder
					stamped = true
				}
			case float64:
				if portValue == float64(operatorWebhookPort) {
					port["port"] = webhookPortPlaceholder
					stamped = true
				}
			}
		}
		if err := unstructured.SetNestedSlice(rule, ports, "ports"); err != nil {
			return "", fmt.Errorf("networkpolicy %s: stamping webhook port: %w", obj.GetName(), err)
		}
	}
	if !stamped {
		// The operator Deployment declares a webhook port but this policy,
		// which selects the operator pod, admits no ingress on it. Emitting
		// the policy with literal ports would silently reintroduce the
		// listener/allowance desync this transform exists to prevent, so
		// fail generation and make the mismatch explicit.
		return "", fmt.Errorf(
			"networkpolicy %s: no ingress rule admits the operator webhook port %d; "+
				"update the policy in config/default to match the Deployment's %q port",
			obj.GetName(), operatorWebhookPort, webhookServerPortName)
	}
	if err := unstructured.SetNestedSlice(obj.Object, ingress, "spec", "ingress"); err != nil {
		return "", fmt.Errorf("networkpolicy %s: stamping webhook port: %w", obj.GetName(), err)
	}

	raw, err := marshalResource(obj)
	if err != nil {
		return "", err
	}
	raw = replaceNamespace(raw)
	raw = strings.ReplaceAll(raw, webhookPortPlaceholder, tplWebhookPort)
	return raw, nil
}

// policySelectsOperator reports whether the policy's podSelector labels are
// all carried by the operator Deployment's pod template.
func policySelectsOperator(obj *unstructured.Unstructured, operatorPodLabels map[string]string) bool {
	if len(operatorPodLabels) == 0 {
		return false
	}
	selector, found, err := unstructured.NestedStringMap(obj.Object, "spec", "podSelector", "matchLabels")
	if err != nil || !found || len(selector) == 0 {
		return false
	}
	for key, value := range selector {
		if operatorPodLabels[key] != value {
			return false
		}
	}
	return true
}

// marshalResource marshals an unstructured resource to YAML.
func marshalResource(obj *unstructured.Unstructured) (string, error) {
	data, err := yaml.Marshal(obj.Object)
	if err != nil {
		return "", fmt.Errorf("marshaling resource: %w", err)
	}

	return strings.TrimSpace(string(data)), nil
}

// replaceNamespace replaces the resource's own metadata.namespace with the
// Helm release namespace template. Scoping is positive, not a blocklist: it
// only ever touches a "namespace:" line found directly inside the
// document's top-level "metadata:" block, never anywhere else in the
// resource. Without that scope, a bare substring/prefix match on
// "namespace:" would also rewrite a RoleBinding subject's own namespace (not
// this function's job -- see stampOperatorSubjectPlaceholders), or corrupt
// an arbitrary ConfigMap/Secret data value that happens to contain the text
// "namespace: <something>" (e.g. embedded YAML/config content in a data
// key), which has nothing to do with the Kubernetes namespace field at all.
func replaceNamespace(raw string) string {
	lines := strings.Split(raw, "\n")
	inMetadata := false

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		if line == yamlFieldMetadata {
			inMetadata = true

			continue
		}
		if inMetadata && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") && trimmed != "" {
			inMetadata = false
		}
		if !inMetadata {
			continue
		}

		// Only a direct child of metadata (sigs.k8s.io/yaml indents those
		// two spaces) is metadata.namespace itself -- anything indented
		// further is inside metadata.labels/annotations, one of which could
		// have its own key or (in a multiline value) embedded text that
		// merely looks like "namespace: ...", and must not be touched.
		indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
		if len(indent) != metadataChildIndent {
			continue
		}

		if strings.HasPrefix(trimmed, yamlFieldNamespace) && !strings.Contains(trimmed, "{{") {
			lines[i] = indent + tplReleaseNamespace
		}
	}

	return strings.Join(lines, "\n")
}

// replaceImageField replaces the manager container's image and
// imagePullPolicy values with Helm template references. It matches by the
// exact sentinel values stampManagerContainerPlaceholders wrote onto that
// specific container -- not by scanning for any "image:"/"imagePullPolicy:"
// line -- so it can't touch a sidecar or init container's own image, and
// isn't sensitive to which field happens to serialize first within a
// container (a bare string-prefix match on "image:" would miss the leading
// "- image: ..." list-item form).
func replaceImageField(raw string) string {
	lines := strings.Split(raw, "\n")
	var result []string

	for _, line := range lines {
		switch {
		case strings.Contains(line, imagePlaceholder):
			prefix := strings.TrimSuffix(line, imagePlaceholder)
			result = append(result, prefix+`"{{ include "chart.imageRef" . }}"`)
		case strings.Contains(line, pullPolicyPlaceholder):
			prefix := strings.TrimSuffix(line, pullPolicyPlaceholder)
			result = append(result, prefix+"{{ .Values.operator.image.pullPolicy }}")
		default:
			result = append(result, line)
		}
	}

	return strings.Join(result, "\n")
}

// replaceReplicas replaces the sentinel transformDeployment stamps onto
// spec.replicas with a Helm template reference. Matching the exact sentinel
// value -- rather than scanning for any "replicas:"-prefixed line -- means
// this can't match embedded YAML-looking text inside an unrelated
// annotation value, and fires even when the source Deployment omitted
// spec.replicas entirely (stamping happens unconditionally, so there's
// always a sentinel to find).
func replaceReplicas(raw string) string {
	return strings.ReplaceAll(raw, replicasPlaceholder, "{{ .Values.operator.replicas }}")
}

// replaceResourcesField expands the manager container's resources sentinel
// (stamped by stampManagerContainerPlaceholders) into a Helm toYaml
// reference. Matching the exact sentinel value -- rather than scanning for
// any bare "resources:" block -- means only the manager container's
// resources are templated (a sidecar's own requests/limits survive
// untouched), and an originally-empty "resources: {}" on the manager, which
// marshals to the same one-line sentinel as a populated map, is templated
// too instead of being silently skipped.
func replaceResourcesField(raw string) string {
	lines := strings.Split(raw, "\n")
	result := make([]string, 0, len(lines)+1)

	for _, line := range lines {
		if !strings.Contains(line, resourcesPlaceholder) {
			result = append(result, line)
			continue
		}

		indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
		result = append(result, indent+"resources:")
		result = append(result, indent+"  {{- toYaml .Values.operator.resources | nindent "+
			fmt.Sprintf("%d", len(indent)+2)+" }}")
	}

	return strings.Join(result, "\n")
}

// replaceServiceAccountName replaces the sentinel transformDeployment stamps
// onto spec.template.spec.serviceAccountName (only when the account is
// chart-managed) with a Helm template reference. If nothing was stamped
// (an externally-managed account), this is a no-op -- the field's real,
// literal value is left exactly as it was, rather than matching it via a
// prefix scan that could also catch unrelated "serviceAccountName:"-looking
// text elsewhere in the document.
func replaceServiceAccountName(raw string) string {
	return strings.ReplaceAll(raw, serviceAccountNamePlaceholder, tplServiceAccountName)
}

// addImagePullSecrets makes .Values.imagePullSecrets (if set) part of the
// pod spec's imagePullSecrets list, anchored on the "imagePullSecrets:" line
// stampImagePullSecretsPlaceholder guarantees is present (as a real key,
// never absent, never the one-line "imagePullSecrets: []" form an empty
// list marshals to).
//
// Two shapes, told apart by whether the very next line is the sentinel
// entry stampImagePullSecretsPlaceholder stamped in:
//   - Real entries were already there: keep the key and those entries, and
//     merge .Values.imagePullSecrets in alongside them.
//   - Nothing was there (the sentinel is the only "entry"): drop the
//     sentinel and the key itself, and only render the key at all inside a
//     "with" guard -- otherwise every chart would render a bare, childless
//     "imagePullSecrets:" key even when nothing overrides it, which used to
//     render nothing at all when the field was genuinely absent.
func addImagePullSecrets(raw string) string {
	lines := strings.Split(raw, "\n")
	result := make([]string, 0, len(lines))

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		if trimmed != "imagePullSecrets:" {
			result = append(result, line)
			continue
		}

		indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
		sentinelOnly := i+1 < len(lines) && strings.Contains(lines[i+1], imagePullSecretsSentinelName)

		if sentinelOnly {
			result = append(result,
				indent+"{{- with .Values.imagePullSecrets }}",
				indent+"imagePullSecrets:",
				indent+"  {{- toYaml . | nindent "+fmt.Sprintf("%d", len(indent)+2)+" }}",
				indent+"{{- end }}",
			)
			i++ // skip the sentinel list item -- it's not a real entry.

			continue
		}

		result = append(result, line)
		result = append(result,
			indent+"{{- with .Values.imagePullSecrets }}",
			indent+"{{- toYaml . | nindent "+fmt.Sprintf("%d", len(indent))+" }}",
			indent+"{{- end }}",
		)
	}

	return strings.Join(result, "\n")
}

// stampImagePullSecretsPlaceholder guarantees the pod spec's
// imagePullSecrets field always marshals as a real, multi-line
// "imagePullSecrets:\n- ..." block -- never absent, never the collapsed
// one-line "imagePullSecrets: []" an empty list serializes as. Both of
// those shapes are textually indistinguishable from "no key at all" to
// addImagePullSecrets' line scan, which used to mean a Deployment with
// imagePullSecrets: [] (or none at all, alongside no serviceAccountName
// field to anchor an insertion point on) silently never got
// .Values.imagePullSecrets merged in -- registry credentials supplied at
// install time had no effect.
func stampImagePullSecretsPlaceholder(obj *unstructured.Unstructured) error {
	existing, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "imagePullSecrets")
	if err != nil {
		return fmt.Errorf("reading imagePullSecrets: %w", err)
	}
	if found && len(existing) > 0 {
		return nil
	}

	sentinel := []any{map[string]any{"name": imagePullSecretsSentinelName}}

	return unstructured.SetNestedSlice(obj.Object, sentinel, "spec", "template", "spec", "imagePullSecrets")
}

// replaceServiceAccountMetadata replaces the ServiceAccount name with the
// Helm value reference and templates in .Values.serviceAccount.annotations.
//
// staticAnnotations distinguishes two shapes: if the source ServiceAccount
// already has static annotations, the entire existing "annotations:" block
// is replaced with a Helm expression that merges .Values.serviceAccount.
// annotations over those same static values (Values wins on a key
// collision, via Sprig's merge(dst, src) -- dst's keys take precedence),
// rather than just inserting the merge lines alongside the untouched static
// YAML. That "insert alongside" shape used to produce two occurrences of a
// colliding key in the rendered YAML; because a YAML/JSON decoder resolves
// duplicate keys by taking whichever occurs last, and the static value
// always ended up last, a Helm override of an existing annotation silently
// had no effect. Only when there's no existing annotations key at all does
// this add a brand new one (after "name:", the same place the old,
// unconditional version always used) with no merge needed.
func replaceServiceAccountMetadata(raw string, originalName string, staticAnnotations map[string]string) string {
	hasAnnotations := len(staticAnnotations) > 0

	lines := strings.Split(raw, "\n")
	result := make([]string, 0, len(lines))

	inMetadata := false

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		if trimmed == yamlFieldMetadata {
			inMetadata = true
		} else if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") && trimmed != "" {
			inMetadata = false
		}

		if inMetadata && trimmed == "annotations:" && hasAnnotations {
			indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
			mergeExpr := "merge (deepCopy (.Values.serviceAccount.annotations | default dict)) (" +
				sprigDictLiteral(staticAnnotations) + ")"
			result = append(result,
				indent+"{{- with "+mergeExpr+" }}",
				indent+"annotations:",
				indent+"  {{- toYaml . | nindent "+fmt.Sprintf("%d", len(indent)+2)+" }}",
				indent+"{{- end }}",
			)

			// Skip the original static annotation entries -- they're now
			// embedded in the dict literal above; leaving them in the
			// output too would recreate the duplicate-key precedence bug
			// this function exists to fix.
			childIndent := len(indent) + 2
			for i+1 < len(lines) {
				next := lines[i+1]
				if strings.TrimSpace(next) == "" {
					i++
					continue
				}
				if len(next)-len(strings.TrimLeft(next, " ")) < childIndent {
					break
				}
				i++
			}

			continue
		}

		if inMetadata && strings.HasPrefix(trimmed, yamlFieldName) && strings.Contains(trimmed, originalName) {
			indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
			result = append(result, indent+yamlFieldName+" "+tplServiceAccountName)

			if !hasAnnotations {
				result = append(result,
					indent+"{{- with .Values.serviceAccount.annotations }}",
					indent+"annotations:",
					indent+"  {{- toYaml . | nindent "+fmt.Sprintf("%d", len(indent)+2)+" }}",
					indent+"{{- end }}",
				)
			}

			continue
		}

		result = append(result, line)
	}

	return strings.Join(result, "\n")
}

// sprigDictLiteral renders m as a Sprig `dict "k1" "v1" "k2" "v2" ...`
// expression, suitable for embedding directly in a Helm template. Keys are
// sorted for deterministic output.
func sprigDictLiteral(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, 1+len(keys)*2)
	parts = append(parts, "dict")
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%q", k), fmt.Sprintf("%q", m[k]))
	}

	return strings.Join(parts, " ")
}

// replaceWebhookNamespace replaces namespace references in webhook
// configurations (clientConfig.service.namespace and cert-manager annotations).
func replaceWebhookNamespace(raw string) string {
	// The resource's own metadata.namespace, if it has one.
	raw = replaceNamespace(raw)

	// webhooks[].clientConfig.service.namespace, stamped by
	// stampWebhookServiceNamespacePlaceholders.
	raw = strings.ReplaceAll(raw, "namespace: "+webhookServiceNamespacePlaceholder, tplReleaseNamespace)

	// Replace namespace in cert-manager inject-ca-from annotation
	lines := strings.Split(raw, "\n")

	for i, line := range lines {
		if strings.Contains(line, annotationCertManagerInjectCAFrom) {
			// The annotation value is typically "namespace/certificate-name"
			// Replace the namespace part with the Helm template
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				value := strings.TrimSpace(parts[1])
				valueParts := strings.SplitN(value, "/", 2)
				if len(valueParts) == 2 {
					indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
					lines[i] = indent + strings.TrimSpace(parts[0]) + ": {{ .Release.Namespace }}/" + valueParts[1]
				}
			}
		}
	}

	return strings.Join(lines, "\n")
}

// injectConfigMapValues adds Helm template directives to write explicit
// platform keys and merge .Values.config into the ConfigMap data.
func injectConfigMapValues(raw string) string {
	lines := strings.Split(raw, "\n")
	var result []string
	i := 0

	for i < len(lines) {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == yamlFieldData {
			indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
			result = append(result, line)
			result = append(result,
				indent+`  platformType: {{ default "OpenDataHub" .Values.platform.type | quote }}`,
				indent+`  platformVersion: {{ default "" .Values.platform.version | quote }}`,
				indent+"  {{- range $key, $val := .Values.config }}",
				indent+`  {{- if and (ne $key "platformType") (ne $key "platformVersion") }}`,
				indent+"  {{ $key }}: {{ $val | quote }}",
				indent+"  {{- end }}",
				indent+"  {{- end }}",
			)

			i++
			dataIndent := len(indent) + 2
			for i < len(lines) {
				next := lines[i]
				if strings.TrimSpace(next) == "" {
					i++
					continue
				}
				nextIndent := len(next) - len(strings.TrimLeft(next, " "))
				if nextIndent >= dataIndent {
					i++
					continue
				}
				break
			}
			continue
		}

		result = append(result, line)
		i++
	}

	return strings.Join(result, "\n")
}

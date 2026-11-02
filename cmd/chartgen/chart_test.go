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
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"
)

func TestWrapOpenShiftAPIGuard(t *testing.T) {
	g := NewWithT(t)
	wrapped := wrapOpenShiftAPIGuard("kind: SecurityContextConstraints\n")
	g.Expect(wrapped).To(HavePrefix(
		"{{- if or .Values.openshift.securityContextConstraints" +
			" (.Capabilities.APIVersions.Has \"security.openshift.io/v1\") }}",
	))
	g.Expect(wrapped).To(ContainSubstring("kind: SecurityContextConstraints"))
	g.Expect(wrapped).To(HaveSuffix("{{- end }}\n"))
}

// The sidecar's "image" field is deliberately its first key (serializes as
// "- image: ...") -- the shape the old line-prefix matcher missed -- while
// the manager container's image is not its first key, the shape it did
// match. A correct implementation must template only the manager
// container's image/imagePullPolicy and leave the sidecar's untouched
// regardless of either container's field order.
const deploymentWithSidecarManifest = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: manager
  namespace: system
spec:
  replicas: 1
  template:
    spec:
      containers:
      - image: quay.io/example/sidecar:v1
        name: proxy
        imagePullPolicy: IfNotPresent
      - name: manager
        command:
        - /manager
        image: controller:latest
        imagePullPolicy: Always
`

func unstructuredFromYAML(t *testing.T, manifest string) unstructured.Unstructured {
	t.Helper()

	var obj unstructured.Unstructured
	if err := yaml.Unmarshal([]byte(manifest), &obj.Object); err != nil {
		t.Fatalf("unmarshaling test manifest: %v", err)
	}

	return obj
}

func TestTransformDeployment_OnlyTemplatesManagerContainerImage(t *testing.T) {
	g := NewWithT(t)

	obj := unstructuredFromYAML(t, deploymentWithSidecarManifest)

	rendered, err := transformDeployment(&obj, false, true)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(rendered).To(ContainSubstring(`image: "{{ include "chart.imageRef" . }}"`))
	g.Expect(rendered).To(ContainSubstring("imagePullPolicy: {{ .Values.operator.image.pullPolicy }}"))

	// The sidecar's own image/pullPolicy, and its image-first field order,
	// must survive untouched.
	g.Expect(rendered).To(ContainSubstring("image: quay.io/example/sidecar:v1"))
	g.Expect(rendered).To(ContainSubstring("imagePullPolicy: IfNotPresent"))

	// The manager's real image/pullPolicy values must not leak into the
	// generated chart -- they're only reachable via the Values templates.
	g.Expect(rendered).NotTo(ContainSubstring("controller:latest"))
	g.Expect(strings.Count(rendered, `{{ include "chart.imageRef" . }}`)).To(Equal(1))
}

func TestTransformDeployment_TemplatesWebhookPortFromConfig(t *testing.T) {
	g := NewWithT(t)

	obj := unstructuredFromYAML(t, `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: manager
spec:
  template:
    spec:
      containers:
      - name: manager
        ports:
        - name: webhook-server
          containerPort: 9443
        - name: health
          containerPort: 8081
        volumeMounts:
        - name: cert
          mountPath: /tmp/k8s-webhook-server/serving-certs
      - name: proxy
        ports:
        - name: webhook-server
          containerPort: 9444
        volumeMounts:
        - name: cert
          mountPath: /proxy/certs
`)

	rendered, err := transformDeployment(&obj, false, true)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rendered).To(ContainSubstring(
		`containerPort: {{ default 9443 (int (index .Values.config "controller.webhook.port")) }}`,
	))
	g.Expect(rendered).To(ContainSubstring("containerPort: 8081"))
	g.Expect(rendered).To(ContainSubstring("containerPort: 9444"))
	g.Expect(rendered).To(ContainSubstring(
		`mountPath: {{ default "` + webhookCertDirDefault + `"`,
	))
	g.Expect(rendered).To(ContainSubstring(
		`(index .Values.config "controller.webhook.cert-dir") | quote }}`,
	))
	g.Expect(rendered).To(ContainSubstring("mountPath: /proxy/certs"))
	g.Expect(strings.Count(rendered, "default 9443")).To(Equal(1))
	g.Expect(strings.Count(rendered, `default "/tmp/k8s-webhook-server/serving-certs"`)).To(Equal(1))
}

// A binding with a mix of subjects -- the operator's own ServiceAccount plus
// an unrelated one, a User, and a Group -- must only have the operator's own
// subject templated. Rewriting every subject in the "subjects:" block
// indiscriminately (the old behavior) would rename the unrelated subjects to
// the operator's own account too, silently transferring the binding's
// permissions to it.
func TestTransformRoleBinding_OnlyTemplatesOperatorSubject(t *testing.T) {
	g := NewWithT(t)

	obj := unstructuredFromYAML(t, `
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: example
subjects:
- kind: ServiceAccount
  name: operator
  namespace: system
- kind: ServiceAccount
  name: some-other-account
  namespace: other-namespace
- kind: User
  name: some-user
- kind: Group
  name: some-group
`)

	rendered, err := transformRoleBinding(&obj, types.NamespacedName{Name: "operator", Namespace: "system"}, true)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(rendered).To(ContainSubstring(`name: {{ default (include "chart.fullname" .) .Values.serviceAccount.name }}`))
	g.Expect(rendered).To(ContainSubstring("name: some-other-account"))
	g.Expect(rendered).To(ContainSubstring("namespace: other-namespace"))
	g.Expect(rendered).To(ContainSubstring("name: some-user"))
	g.Expect(rendered).To(ContainSubstring("name: some-group"))
}

// A ServiceAccount named the same as the operator's own but in a different
// namespace is a different principal -- matching by name alone would rename
// it (and its own, unrelated namespace) to the operator's identity, handing
// this binding's permissions to the real operator account instead.

func TestTransformNetworkPolicy_TemplatesWebhookPortForOperatorPolicy(t *testing.T) {
	g := NewWithT(t)

	obj := unstructuredFromYAML(t, `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: controller-manager
  namespace: system
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: opendatahub-db-operator
  policyTypes:
  - Ingress
  - Egress
  ingress:
  - from:
    - namespaceSelector:
        matchExpressions:
        - key: kubernetes.io/metadata.name
          operator: In
          values: [openshift-monitoring]
    ports:
    - port: 8443
      protocol: TCP
  - from:
    - namespaceSelector:
        matchLabels:
          policy-group.network.openshift.io/host-network: ""
    ports:
    - port: 9999
      protocol: TCP
  egress:
  - {}
`)

	// A non-default port (9999) proves the transform matches the port the
	// operator Deployment declares, not a hardcoded default.
	rendered, err := transformNetworkPolicy(&obj, map[string]string{
		"app.kubernetes.io/name": "opendatahub-db-operator",
	}, 9999)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rendered).To(ContainSubstring(
		`port: {{ default 9443 (int (index .Values.config "controller.webhook.port")) }}`),
		"the webhook ingress port must template from the same chart value as the Deployment")
	g.Expect(rendered).To(ContainSubstring("port: 8443"),
		"the metrics port is not chart-configurable and must stay literal")
	g.Expect(strings.Count(rendered, "default 9443")).To(Equal(1))
}

func TestTransformNetworkPolicy_LeavesForeignPolicyAlone(t *testing.T) {
	g := NewWithT(t)

	// A policy for some other workload that happens to allow 9443: its
	// ports must stay literal, or the operator's webhook value would leak
	// into an unrelated policy's rules.
	obj := unstructuredFromYAML(t, `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: some-other-policy
  namespace: system
spec:
  podSelector:
    matchLabels:
      app: something-else
  ingress:
  - ports:
    - port: 9443
      protocol: TCP
`)

	rendered, err := transformNetworkPolicy(&obj, map[string]string{
		"app.kubernetes.io/name": "opendatahub-db-operator",
	}, 9443)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rendered).To(ContainSubstring("port: 9443"))
	g.Expect(rendered).NotTo(ContainSubstring("default 9443"))
}

func TestOperatorWebhookContainerPort(t *testing.T) {
	g := NewWithT(t)

	deployment := unstructuredFromYAML(t, `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: manager
spec:
  template:
    spec:
      containers:
      - name: manager
        ports:
        - name: webhook-server
          containerPort: 9443
        - name: health
          containerPort: 8081
      - name: proxy
        ports:
        - name: webhook-server
          containerPort: 9444
`)
	g.Expect(operatorWebhookContainerPort(&deployment)).To(Equal(int64(9443)),
		"the first webhook-server named port is the manager's")

	noWebhook := unstructuredFromYAML(t, `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: manager
spec:
  template:
    spec:
      containers:
      - name: manager
        ports:
        - name: health
          containerPort: 8081
`)
	g.Expect(operatorWebhookContainerPort(&noWebhook)).To(Equal(int64(0)),
		"a Deployment without a webhook-server port must yield 0, leaving NetworkPolicy ports literal")
}

func TestOperatorWebhookContainerPort_SidecarFirstIsIgnored(t *testing.T) {
	g := NewWithT(t)

	// A sidecar declaring its own webhook-server port, listed BEFORE the
	// manager: the extraction must still return the manager's port, matching
	// the container the Deployment transform templates.
	deployment := unstructuredFromYAML(t, `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: manager
spec:
  template:
    spec:
      containers:
      - name: kube-auth-proxy
        ports:
        - name: webhook-server
          containerPort: 9444
      - name: manager
        ports:
        - name: webhook-server
          containerPort: 9443
`)
	g.Expect(operatorWebhookContainerPort(&deployment)).To(Equal(int64(9443)),
		"only the manager container's webhook-server port may be extracted")
}

func TestTransformRoleBinding_IgnoresSameNameDifferentNamespace(t *testing.T) {
	g := NewWithT(t)

	obj := unstructuredFromYAML(t, `
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: example
subjects:
- kind: ServiceAccount
  name: operator
  namespace: some-other-namespace
`)

	rendered, err := transformRoleBinding(&obj, types.NamespacedName{Name: "operator", Namespace: "system"}, true)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(rendered).NotTo(ContainSubstring(`{{ default (include "chart.fullname" .) .Values.serviceAccount.name }}`))
	g.Expect(rendered).To(ContainSubstring("name: operator"))
	g.Expect(rendered).To(ContainSubstring("namespace: some-other-namespace"))
}

// Matching any bare "resources:" block in the whole Deployment (the old
// behavior) would template a sidecar's own resource requests/limits away
// and, separately, skip the manager container entirely when its resources
// were an empty {} (a single-line block, never matched at all). Both must
// be fixed by the same structural, sentinel-based approach used for image.
func TestTransformDeployment_OnlyTemplatesManagerContainerResources(t *testing.T) {
	g := NewWithT(t)

	obj := unstructuredFromYAML(t, `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: manager
spec:
  replicas: 1
  template:
    spec:
      containers:
      - name: proxy
        image: quay.io/example/sidecar:v1
        resources:
          limits:
            memory: 64Mi
          requests:
            memory: 32Mi
      - name: manager
        image: controller:latest
        resources: {}
`)

	rendered, err := transformDeployment(&obj, false, true)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(rendered).To(ContainSubstring(`{{- toYaml .Values.operator.resources | nindent`))
	// The sidecar's own resources must survive untouched.
	g.Expect(rendered).To(ContainSubstring("memory: 64Mi"))
	g.Expect(rendered).To(ContainSubstring("memory: 32Mi"))
}

// A ServiceAccount that already has static annotations must keep them, and
// the Helm-override merge must land inside that same "annotations:" key --
// not as a second, sibling "annotations:" key, which the old unconditional
// append always produced.
func TestTransformServiceAccount_MergesIntoExistingAnnotations(t *testing.T) {
	g := NewWithT(t)

	obj := unstructuredFromYAML(t, `
apiVersion: v1
kind: ServiceAccount
metadata:
  name: operator
  namespace: system
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/example
`)

	rendered, err := transformServiceAccount(&obj)
	g.Expect(err).NotTo(HaveOccurred())

	// The static annotation must be embedded in the merge's dict literal --
	// not also left behind as literal YAML alongside it. A duplicate-key
	// YAML/JSON document resolves to whichever occurrence comes last, so
	// leaving the static line in place (the old behavior) would make it
	// silently win over any conflicting Helm override, the exact bug this
	// merge exists to fix.
	g.Expect(strings.Count(rendered, "annotations:")).To(Equal(1), "rendered:\n%s", rendered)
	g.Expect(rendered).To(ContainSubstring(`dict "eks.amazonaws.com/role-arn" "arn:aws:iam::123456789012:role/example"`))
	g.Expect(rendered).To(ContainSubstring("merge (deepCopy (.Values.serviceAccount.annotations | default dict))"))
	g.Expect(rendered).NotTo(MatchRegexp(`(?m)^\s+eks\.amazonaws\.com/role-arn:\s+arn:aws:iam`),
		"static annotation must not also appear as literal YAML, or it would win the duplicate-key precedence")
}

// A Helm upgrade that only changes .Values.platform/.Values.config changes
// the ConfigMap's rendered data but nothing else in the Deployment -- with
// no checksum annotation, the pod template hash doesn't change, so already
// running pods (which read config once at startup) never pick up the
// change. The checksum annotation must be present whenever the operator has
// a ConfigMap, and absent when it doesn't (nothing to check).
func TestTransformDeployment_AddsConfigChecksumAnnotationWhenConfigMapPresent(t *testing.T) {
	g := NewWithT(t)

	obj := unstructuredFromYAML(t, deploymentWithSidecarManifest)

	rendered, err := transformDeployment(&obj, true, true)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rendered).To(ContainSubstring(
		`checksum/config: {{ include (print $.Template.BasePath "/core_v1_configmap.yaml") . | sha256sum }}`,
	))
}

func TestTransformDeployment_OmitsConfigChecksumAnnotationWithoutConfigMap(t *testing.T) {
	g := NewWithT(t)

	obj := unstructuredFromYAML(t, deploymentWithSidecarManifest)

	rendered, err := transformDeployment(&obj, false, true)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rendered).NotTo(ContainSubstring("checksum/config"))
}

// replaceNamespace only ever touches a resource's own top-level
// metadata.namespace (by design), so a webhook's target Service namespace --
// nested under webhooks[].clientConfig.service.namespace -- needs its own,
// still-structural mechanism to get rewritten at all.
func TestTransformWebhook_RewritesServiceNamespace(t *testing.T) {
	g := NewWithT(t)

	obj := unstructuredFromYAML(t, `
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata:
  name: example
webhooks:
- name: validate.example.com
  clientConfig:
    service:
      name: webhook-service
      namespace: system
      path: /validate
`)

	rendered, err := transformWebhook(&obj)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rendered).To(ContainSubstring("namespace: {{ .Release.Namespace }}"))
	g.Expect(rendered).NotTo(ContainSubstring("namespace: system"))
}

// A Deployment that already declares imagePullSecrets (private registry
// credentials set directly in the kustomize input) must keep them -- and
// gain any Helm-supplied ones alongside, under the same key -- rather than
// getting a second, sibling "imagePullSecrets:" key that would let one set
// silently shadow the other once decoded.
func TestTransformDeployment_MergesIntoExistingImagePullSecrets(t *testing.T) {
	g := NewWithT(t)

	obj := unstructuredFromYAML(t, `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: manager
spec:
  replicas: 1
  template:
    spec:
      imagePullSecrets:
      - name: existing-registry-secret
      containers:
      - name: manager
        image: controller:latest
`)

	rendered, err := transformDeployment(&obj, false, true)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(strings.Count(rendered, "imagePullSecrets:")).To(Equal(1), "rendered:\n%s", rendered)
	g.Expect(rendered).To(ContainSubstring("name: existing-registry-secret"))
	g.Expect(rendered).To(ContainSubstring(`{{- with .Values.imagePullSecrets }}`))
}

// An explicitly empty imagePullSecrets: [] (or the field being absent
// entirely) marshals to a form the old line-scan couldn't tell apart from
// "no key at all" -- and since insertion of a brand new key was itself
// anchored on finding serviceAccountName, a Deployment with neither field
// never got .Values.imagePullSecrets merged in at all. Both shapes must
// still produce a working merge, and never leave the sentinel entry behind.
func TestTransformDeployment_HandlesEmptyOrAbsentImagePullSecrets(t *testing.T) {
	cases := map[string]string{
		"empty list": `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: manager
spec:
  replicas: 1
  template:
    spec:
      imagePullSecrets: []
      containers:
      - name: manager
        image: controller:latest
`,
		"absent, and no serviceAccountName to anchor on either": `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: manager
spec:
  replicas: 1
  template:
    spec:
      containers:
      - name: manager
        image: controller:latest
`,
	}

	for name, manifest := range cases {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)

			obj := unstructuredFromYAML(t, manifest)

			rendered, err := transformDeployment(&obj, false, true)
			g.Expect(err).NotTo(HaveOccurred())
			// The key itself must only render when Values actually supplies
			// something -- i.e. it must appear on the line right after the
			// "with" guard opens, not unconditionally before it.
			g.Expect(rendered).To(MatchRegexp(`(?m)^\s*\{\{- with \.Values\.imagePullSecrets \}\}\n\s*imagePullSecrets:`),
				"rendered:\n%s", rendered)
			g.Expect(rendered).NotTo(ContainSubstring(imagePullSecretsSentinelName))
		})
	}
}

// injectConfigMapValues only fired on a line reading exactly "data:" -- an
// absent data field (no such line at all) or an explicitly empty one
// (marshals to the one-line "data: {}", never a "data:" heading) used to
// skip the Helm-templating injection entirely, leaving Values.platform/
// Values.config with no effect on the rendered ConfigMap.
func TestTransformConfigMap_InjectsValuesWhenDataAbsentOrEmpty(t *testing.T) {
	cases := map[string]string{
		"absent data field": `
apiVersion: v1
kind: ConfigMap
metadata:
  name: config
`,
		"explicitly empty data field": `
apiVersion: v1
kind: ConfigMap
metadata:
  name: config
data: {}
`,
	}

	for name, manifest := range cases {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)

			obj := unstructuredFromYAML(t, manifest)

			rendered, err := transformConfigMap(&obj)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(rendered).To(ContainSubstring(`platformType: {{ default "OpenDataHub" .Values.platform.type | quote }}`))
			g.Expect(rendered).NotTo(ContainSubstring(configMapDataSentinelKey))
		})
	}
}

// A bare "replicas:" prefix scan would also match embedded YAML-looking
// text inside an unrelated annotation value, and would never fire at all
// when spec.replicas is omitted (a valid Deployment can leave it unset,
// defaulting to 1) -- silently making .Values.operator.replicas have no
// effect. Both cases must still produce a working template reference, and
// the annotation's own literal text must survive untouched.
func TestTransformDeployment_TemplatesReplicasStructurally(t *testing.T) {
	cases := map[string]string{
		"omitted spec.replicas": `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: manager
  annotations:
    example.com/note: "replicas: not-a-real-field"
spec:
  template:
    spec:
      containers:
      - name: manager
        image: controller:latest
`,
		"explicit spec.replicas": `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: manager
  annotations:
    example.com/note: "replicas: not-a-real-field"
spec:
  replicas: 3
  template:
    spec:
      containers:
      - name: manager
        image: controller:latest
`,
	}

	for name, manifest := range cases {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)

			obj := unstructuredFromYAML(t, manifest)

			rendered, err := transformDeployment(&obj, false, true)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(rendered).To(ContainSubstring("replicas: {{ .Values.operator.replicas }}"))
			g.Expect(rendered).To(ContainSubstring(`example.com/note: 'replicas: not-a-real-field'`),
				"the annotation's own literal text must survive untouched:\n%s", rendered)
		})
	}
}

func TestTransformDeployment_ErrorsWithoutManagerContainer(t *testing.T) {
	g := NewWithT(t)

	obj := unstructuredFromYAML(t, `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: manager
spec:
  template:
    spec:
      containers:
      - name: not-the-manager
        image: controller:latest
`)

	_, err := transformDeployment(&obj, false, true)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("manager"))
}

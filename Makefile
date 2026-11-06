# Include local overrides if present (gitignored).
-include local.mk

# Image URL to use for all building/pushing image targets.
IMG ?= quay.io/opendatahub/odh-db-operator:latest
# YEAR defines the year value used for substituting the YEAR placeholder in the boilerplate header.
# Keep generated file headers stable across calendar years.
YEAR ?= 2026

# This repo builds container images with podman, not docker.
CONTAINER_TOOL ?= podman

# Setting SHELL to bash allows bash commands to be executed by recipes.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

## Tool Binaries -- installed into LOCALBIN at pinned versions so local and
## CI toolchains match; override on the command line (e.g. `make lint
## GOLANGCI_LINT=golangci-lint`) to use a binary from PATH instead.
LOCALBIN ?= $(shell pwd)/bin
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
KUSTOMIZE      ?= $(LOCALBIN)/kustomize
GOLANGCI_LINT  ?= $(LOCALBIN)/golangci-lint
HELM           ?= $(LOCALBIN)/helm
ENVTEST        ?= $(LOCALBIN)/setup-envtest
KUBECTL        ?= kubectl
KIND           ?= $(LOCALBIN)/kind

## Tool Versions
CONTROLLER_GEN_VERSION ?= v0.17.0
KUSTOMIZE_VERSION ?= v5.5.0
GOLANGCI_LINT_VERSION ?= v2.6.0
HELM_VERSION ?= v3.19.0
SETUP_ENVTEST_VERSION ?= release-0.20
KIND_VERSION ?= v0.33.0

GO_VERSION ?= $(shell sed -n 's/^go //p' go.mod)

$(LOCALBIN):
	mkdir -p $(LOCALBIN)

# go-install-tool 'go install's a pinned package and symlinks the version-stamped
# binary to $(1), so bumping a version re-triggers installation.
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" || true ;\
GOTOOLCHAIN="go$(GO_VERSION)" GOBIN="$(LOCALBIN)" go install "$${package}" ;\
mv "$(1)" "$(1)-$(3)" ;\
} ;\
[ "$$(readlink "$(1)")" = "$(1)-$(3)" ] || ln -sf "$(1)-$(3)" "$(1)"
endef

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_GEN_VERSION))

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

.PHONY: helm-tool
helm-tool: $(HELM) ## Download helm locally if necessary.
$(HELM): $(LOCALBIN)
	$(call go-install-tool,$(HELM),helm.sh/helm/v3/cmd/helm,$(HELM_VERSION))

.PHONY: kind
kind: $(KIND) ## Download kind locally if necessary.
$(KIND): $(LOCALBIN)
	$(call go-install-tool,$(KIND),sigs.k8s.io/kind,$(KIND_VERSION))

.PHONY: kind-create
kind-create: kind ## Create the dev Kind cluster the e2e/integration targets expect.
	$(KIND) create cluster --name "$(KIND_CLUSTER)" --wait 5m

.PHONY: kind-delete
kind-delete: kind ## Delete the dev Kind cluster.
	$(KIND) delete cluster --name "$(KIND_CLUSTER)"

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(SETUP_ENVTEST_VERSION))

# Version of the envtest control-plane binaries (etcd/kube-apiserver) to use
# for the envtest suite under test/envtest. This is the single source of truth;
# CI runs `make test-envtest` without overrides.
ENVTEST_K8S_VERSION ?= 1.31.0

.PHONY: all
all: build

##@ General

.PHONY: print-kind-version
print-kind-version: ## Print the pinned kind version (for CI).
	@echo $(KIND_VERSION)

.PHONY: print-cert-manager-version
print-cert-manager-version: ## Print the pinned cert-manager chart version (for CI).
	@echo $(CERT_MANAGER_VERSION)

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate the manager ClusterRole and CRDs.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases
	# Sync the embedded operand manifests with the generated CRD bases; the
	# operands sync test fails when this step is skipped.
	cp config/crd/bases/infrastructure.opendatahub.io_databaseclaims.yaml \
		config/crd/bases/infrastructure.opendatahub.io_databaseproviders.yaml \
		config/crd/bases/infrastructure.opendatahub.io_schemaclaims.yaml \
		internal/controller/databaseservice/operands/manifests/

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

.PHONY: fmt
fmt: golangci-lint ## Run golangci-lint formatters against code.
	GOTOOLCHAIN=go$(GO_VERSION) $(GOLANGCI_LINT) fmt

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: deps
deps: ## Tidy and verify Go module dependencies.
	go mod tidy
	go mod verify

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter.
	# GOTOOLCHAIN pins the go command golangci-lint shells out to, to the go.mod
	# version: a newer local go feeds it dependency files built for go1.27+ that
	# its embedded typechecker (built with go.mod's version) cannot load.
	GOTOOLCHAIN=go$(GO_VERSION) $(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes.
	GOTOOLCHAIN=go$(GO_VERSION) $(GOLANGCI_LINT) run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration.
	$(GOLANGCI_LINT) config verify

UNIT_TEST_PACKAGES ?= ./api/... ./assets/... ./cmd/... ./internal/... ./pkg/... ./test/support/...

.PHONY: test
test: test-unit ## Run unit tests.

.PHONY: test-unit
test-unit: ## Run unit tests in the selected packages without a cluster.
	go test $(UNIT_TEST_PACKAGES) -coverprofile cover.out

.PHONY: test-envtest
test-envtest: manifests envtest ## Run the envtest-based reconciler suite (no external cluster required).
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" \
		go test ./test/envtest/... -v -timeout 5m -failfast

.PHONY: test-integration-setup
test-integration-setup: manifests kustomize ## Prepare the cluster for integration tests.
	@context="$$($(KUBECTL) config current-context)"; \
	if [ "$$context" != "$(TEST_INTEGRATION_CONTEXT)" ]; then \
		echo "integration setup requires kube context $(TEST_INTEGRATION_CONTEXT) (current: $$context)" >&2; \
		exit 1; \
	fi
	$(KUSTOMIZE) build config/crd/bases | $(KUBECTL) apply -f -

.PHONY: test-integration-run
test-integration-run: ## Run integration tests against the current kubeconfig context's cluster.
	# -count=1 bypasses go's test cache: this suite asserts live cluster state,
	# so a cached result from a previous run against another cluster would be
	# a false PASS.
	go test ./test/integration/... -v -count=1 -timeout 5m -failfast

.PHONY: test-integration
test-integration: test-integration-setup test-integration-run ## Set up and run integration tests.

.PHONY: test-e2e-setup
test-e2e-setup: test-e2e-context-check helm-tool ## Install cert-manager when needed, build/load the manager image, and install the Helm chart into Kind.
	@if [ "$(E2E_CERT_MANAGER_PROFILE)" != absent ]; then $(MAKE) test-e2e-cert-manager; fi
	$(KUBECTL) get --raw=/readyz >/dev/null
	$(MAKE) test-e2e-teardown
	# Helm does not update CRDs; include module operand CRDs the standalone chart does not package.
	$(KUBECTL) apply -f config/crd/bases/services.platform.opendatahub.io_databaseservices.yaml \
		-f config/crd/bases/infrastructure.opendatahub.io_databaseproviders.yaml \
		-f config/crd/bases/infrastructure.opendatahub.io_databaseclaims.yaml \
		-f config/crd/bases/infrastructure.opendatahub.io_schemaclaims.yaml
	$(KUBECTL) wait --for=condition=Established --timeout=60s \
		crd/databaseservices.services.platform.opendatahub.io \
		crd/databaseproviders.infrastructure.opendatahub.io \
		crd/databaseclaims.infrastructure.opendatahub.io \
		crd/schemaclaims.infrastructure.opendatahub.io
	$(MAKE) container-build IMG="$(IMG)"
	$(MAKE) container-load-kind IMG="$(IMG)" KIND_CLUSTER="$(KIND_CLUSTER)"
# The bootstrap webhook certificate needs cert-manager; provider/claim tests do not.
	@cleanup() { \
		setup_result=$$?; trap - EXIT; \
		if [ -n "$$temporary_chart" ] && ! rm -rf "$$temporary_chart"; then setup_result=1; fi; \
		if [ "$$setup_result" -ne 0 ] && [ "$$namespace_owned" -eq 1 ]; then \
			$(KUBECTL) delete namespace "$(E2E_NAMESPACE)" --ignore-not-found --wait --timeout=5m || \
				echo "e2e setup failed and namespace cleanup also failed" >&2; \
		fi; \
		exit "$$setup_result"; \
	}; \
	chart=config/chart; temporary_chart=""; namespace_owned=0; trap cleanup EXIT; \
	if [ "$(E2E_CERT_MANAGER_PROFILE)" = absent ]; then \
		temporary_chart="$$(mktemp -d)"; \
		cp -R config/chart/. "$$temporary_chart/"; \
		rm "$$temporary_chart/templates/cert-manager.io_v1_issuer.yaml" \
			"$$temporary_chart/templates/cert-manager.io_v1_certificate.yaml"; \
		$(KUBECTL) create namespace "$(E2E_NAMESPACE)"; \
		namespace_owned=1; \
		$(KUBECTL) create secret generic webhook-server-cert \
			--namespace "$(E2E_NAMESPACE)" \
			--from-literal=tls.crt=unused --from-literal=tls.key=unused; \
		chart="$$temporary_chart"; \
		set -- --set-string 'config.controller\.webhook\.enabled=false'; \
	else \
		namespace_owned=1; \
	fi; \
		$(HELM) install "$(E2E_RELEASE)" "$$chart" \
			--namespace "$(E2E_NAMESPACE)" --create-namespace --atomic --wait --timeout 5m \
			--set-string operator.image.ref="$(IMG)" \
			--set-string operator.image.pullPolicy=IfNotPresent \
			--set monitoring.serviceMonitor.enabled=false \
			--set-string config.postgres-image="$(E2E_INTERNAL_POSTGRES_IMAGE)" "$$@"

.PHONY: test-e2e-run
test-e2e-run: test-e2e-context-check ## Verify the Helm-deployed manager reconciles DatabaseService, then clean up.
	$(KUBECTL) get --raw=/readyz >/dev/null
	# -count=1 bypasses go's test cache: this suite asserts live cluster state,
	# so a cached result from a previous run against another cluster would be
	# a false PASS.
	@cleanup() { \
		test_result=$$?; \
		trap - EXIT; \
		cleanup_result=0; \
		$(MAKE) test-e2e-teardown || cleanup_result=$$?; \
		if [ "$$test_result" -ne 0 ]; then exit "$$test_result"; fi; \
		exit "$$cleanup_result"; \
	}; \
	trap cleanup EXIT; \
	ODH_E2E_NAMESPACE="$(E2E_NAMESPACE)" \
	ODH_E2E_RELEASE="$(E2E_RELEASE)" \
	ODH_E2E_OPERATOR_IMAGE="$(IMG)" \
	ODH_E2E_CERT_MANAGER_PROFILE="$(E2E_CERT_MANAGER_PROFILE)" \
	ODH_E2E_EXTERNAL_ADMIN_SECRET_NAMESPACE="$(E2E_EXTERNAL_ADMIN_SECRET_NAMESPACE)" \
	ODH_E2E_EXTERNAL_ADMIN_SECRET_NAME="$(E2E_EXTERNAL_ADMIN_SECRET_NAME)" \
	ODH_E2E_CONSUMER_IMAGE="$(E2E_CONSUMER_IMAGE)" \
		go test ./test/e2e/... -v -timeout 20m

.PHONY: test-e2e-teardown
test-e2e-teardown: test-e2e-context-check helm-tool ## Remove the e2e DatabaseService, Helm release, and namespace.
	@cleanup_status=0; \
	if $(KUBECTL) get crd databaseservices.services.platform.opendatahub.io >/dev/null 2>&1; then \
		$(KUBECTL) delete databaseservice default-db-operator --ignore-not-found --wait || cleanup_status=$$?; \
	fi; \
	$(HELM) uninstall "$(E2E_RELEASE)" --namespace "$(E2E_NAMESPACE)" --ignore-not-found --wait --timeout 5m || cleanup_status=$$?; \
	$(KUBECTL) delete namespace "$(E2E_NAMESPACE)" --ignore-not-found --wait --timeout=5m || cleanup_status=$$?; \
	exit "$$cleanup_status"

.PHONY: test-e2e-context-check
test-e2e-context-check:
	@context="$$($(KUBECTL) config current-context)"; \
	expected="kind-$(KIND_CLUSTER)"; \
	if [ "$$context" != "$$expected" ]; then \
		echo "e2e requires kube context $$expected (current: $$context)" >&2; \
		exit 1; \
	fi

.PHONY: test-e2e-cert-manager
test-e2e-cert-manager: test-e2e-context-check ## Install cert-manager into the Kind cluster if absent, then wait for it.
	@manifest="$$(mktemp -d)/cert-manager.yaml"; \
	cleanup() { rm -rf "$${manifest%/*}"; }; trap cleanup EXIT; \
	if $(KUBECTL) get crd certificates.cert-manager.io >/dev/null 2>&1; then \
		echo "cert-manager already installed; skipping"; \
	else \
		echo "installing cert-manager $(CERT_MANAGER_VERSION)"; \
		if ! curl --fail --location --silent --show-error --output "$$manifest" \
			"https://github.com/cert-manager/cert-manager/releases/download/$(CERT_MANAGER_VERSION)/cert-manager.yaml"; then \
			exit 1; \
		fi; \
		if ! printf '%s  %s\n' "$(CERT_MANAGER_SHA256)" "$$manifest" | shasum -a 256 --check --status; then \
			echo "cert-manager manifest digest mismatch for $(CERT_MANAGER_VERSION)" >&2; \
			exit 1; \
		fi; \
		$(KUBECTL) apply -f "$$manifest"; \
	fi
	$(KUBECTL) wait --for=condition=Established crd/certificates.cert-manager.io --timeout=60s
	if [ -z "$$($(KUBECTL) get deployments -A --selector=app.kubernetes.io/instance=cert-manager -o name)" ]; then \
		echo "no cert-manager Deployments found in any namespace" >&2; exit 1; \
	fi
	$(KUBECTL) wait --for=condition=Available deployment --all --all-namespaces \
		--selector=app.kubernetes.io/instance=cert-manager --timeout=300s

.PHONY: test-e2e
test-e2e: ## Set up and run e2e tests.
	$(MAKE) test-e2e-setup
	$(MAKE) test-e2e-run

.PHONY: test-e2e-providers
test-e2e-providers: ## Run the Internal and External provider lifecycles on a Kind cluster.
	@test -n "$(E2E_EXTERNAL_ADMIN_SECRET_NAMESPACE)" -a -n "$(E2E_EXTERNAL_ADMIN_SECRET_NAME)" || { \
		echo "set E2E_EXTERNAL_ADMIN_SECRET_NAMESPACE and E2E_EXTERNAL_ADMIN_SECRET_NAME" >&2; exit 1; \
	}
	@case "$(E2E_CERT_MANAGER_PROFILE)" in present|absent) ;; *) \
		echo "set E2E_CERT_MANAGER_PROFILE to present or absent" >&2; exit 1 ;; \
	esac
	$(MAKE) test-e2e-context-check
	$(MAKE) test-e2e

##@ Build

VERSION     ?= 0.0.0-dev
GIT_COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
GOOS        ?= $(shell go env GOOS)
GOARCH      ?= $(shell go env GOARCH)
CGO_ENABLED ?= 0
BIN_DIR     ?= bin
BIN_NAME    ?= manager

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	mkdir -p "$(BIN_DIR)"
	CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH) \
		go build -o "$(BIN_DIR)/$(BIN_NAME)" ./cmd/main.go

.PHONY: build-bin
build-bin: ## Build manager binary only, no codegen (for Containerfile; run manifests/generate on host first).
	mkdir -p "$(BIN_DIR)"
	CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH) \
		go build -o "$(BIN_DIR)/$(BIN_NAME)" ./cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	ODH_MODULE_OPERATOR_CONTROLLER_LEADER_ELECTION_ENABLED=false go run ./cmd/main.go operator

.PHONY: container-build
container-build: ## Build container image with the manager (podman).
	$(CONTAINER_TOOL) build -f Containerfile -t "$(IMG)" .

.PHONY: container-push
container-push: ## Push container image with the manager.
	$(CONTAINER_TOOL) push "$(IMG)"

E2E_IMG ?= quay.io/rhoai/odh-db-operator-e2e-rhel9:dev
E2E_CONTEXT ?=
E2E_ARTIFACTS ?= $(CURDIR)/bin/e2e-artifacts
E2E_TEST_PATTERN ?= ^TestHelmInstalledManagerReconcilesDatabaseService$$

.PHONY: e2e-image-build
e2e-image-build: ## Build the e2e test image (context: repo root).
	"$(CONTAINER_TOOL)" build -f test/e2e/Dockerfile -t "$(E2E_IMG)" .

.PHONY: e2e-image-run
e2e-image-run: ## Run the e2e image lifecycle on E2E_CONTEXT and save reports locally.
	@CONTAINER_TOOL="$(CONTAINER_TOOL)" KUBECTL="$(KUBECTL)" \
		E2E_IMG="$(E2E_IMG)" E2E_CONTEXT="$(E2E_CONTEXT)" \
		E2E_ARTIFACTS="$(E2E_ARTIFACTS)" E2E_TEST_PATTERN='$(E2E_TEST_PATTERN)' \
		ODH_E2E_OPERATOR_IMAGE="$(IMG)" ODH_E2E_NAMESPACE="$(E2E_NAMESPACE)" \
		ODH_E2E_RELEASE="$(E2E_RELEASE)" \
		bash test/e2e/scripts/run-image.sh

KIND_CLUSTER ?= db-operator-dev
TEST_INTEGRATION_CONTEXT ?= kind-$(KIND_CLUSTER)
E2E_NAMESPACE ?= opendatahub-db-operator-e2e
E2E_RELEASE   ?= opendatahub-db-operator-e2e
# Installed by test-e2e-cert-manager when absent: the chart's webhook serving
# certificate and internal-provider TLS both require cert-manager on the cluster.
override CERT_MANAGER_VERSION := v1.20.4
override CERT_MANAGER_SHA256 := c5ad170bd564b37d4bd11fd2c52013e64c2a8266417eb71efc318510a69455e6
E2E_CERT_MANAGER_PROFILE ?=
E2E_EXTERNAL_ADMIN_SECRET_NAMESPACE ?=
E2E_EXTERNAL_ADMIN_SECRET_NAME ?=
E2E_CONSUMER_IMAGE ?= postgres:16
E2E_INTERNAL_POSTGRES_IMAGE ?= quay.io/sclorg/postgresql-16-c9s@sha256:66566a27e08ec28d739c5fc54dcd47a4f0b505682d469589e9d493ae0b7bc9ab

.PHONY: container-load-kind
container-load-kind: kind ## Load $(IMG) into a Kind cluster. `kind load docker-image` doesn't work with the podman provider.
	tmp="$$(mktemp)"; trap 'rm -f "$$tmp" "$$tmp.tar"' EXIT; \
	$(CONTAINER_TOOL) save "$(IMG)" -o "$$tmp.tar"; \
	$(KIND) load image-archive "$$tmp.tar" --name "$(KIND_CLUSTER)"

##@ Helm

.PHONY: helm
helm: manifests generate kustomize helm-tool ## Generate and lint the Helm chart from kustomize output via chartgen.
	# Chartgen stages output before replacing chart artifacts; do not delete config/chart before running it.
	# Merge OpenShift install-time SCC/ClusterRole (no namePrefix) after config/default.
	{ $(KUSTOMIZE) build config/default; echo '---'; $(KUSTOMIZE) build config/openshift; } | \
		go run ./cmd/main.go chartgen --output config/chart
	$(HELM) lint config/chart

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( $(KUSTOMIZE) build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | $(KUBECTL) apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config.
	@out="$$( $(KUSTOMIZE) build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config, via kustomize.
	cd config/manager && $(KUSTOMIZE) edit set image controller="$(IMG)"
	$(KUSTOMIZE) build config/default | $(KUBECTL) apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config.
	# config/default's bundle includes the operator's Namespace (kubebuilder's
	# default scaffold shape). Piping the whole bundle straight to `kubectl
	# delete -f -` would delete that Namespace object -- and, cascading from
	# it, every resource in it, including anything created independently of
	# this chart/kustomization (Secrets, other workloads, PVCs). The awk
	# filter below drops the Namespace document before it ever reaches
	# `kubectl delete`, so `make undeploy` only ever removes the resources
	# this kustomization actually manages.
	$(KUSTOMIZE) build config/default | awk ' \
		BEGIN { doc = ""; isns = 0 } \
		/^kind: Namespace$$/ { isns = 1 } \
		/^---$$/ { \
			if (!isns && doc != "") printf "%s---\n", doc; \
			doc = ""; isns = 0; \
			next; \
		} \
		{ doc = doc $$0 "\n" } \
		END { if (!isns && doc != "") printf "%s", doc } \
	' | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

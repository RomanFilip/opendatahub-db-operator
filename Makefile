# Include local overrides if present (gitignored).
-include local.mk

# Image URL to use for all building/pushing image targets.
IMG ?= quay.io/opendatahub/opendatahub-db-operator:latest
# YEAR defines the year value used for substituting the YEAR placeholder in the boilerplate header.
# Keep generated file headers stable across calendar years.
YEAR ?= 2026

# This repo builds container images with podman, not docker.
CONTAINER_TOOL ?= podman

# Setting SHELL to bash allows bash commands to be executed by recipes.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

## Tool Commands -- this repo's dev environment installs these on PATH at
## pinned versions (see docs) rather than `go run`-ing a versioned module per
## invocation. Override on the command line (e.g. `make lint GOLANGCI_LINT=...`)
## to point at a different binary.
CONTROLLER_GEN ?= controller-gen
KUSTOMIZE      ?= kustomize
GOLANGCI_LINT  ?= golangci-lint
HELM           ?= helm
KUBECTL        ?= kubectl
KIND           ?= kind
ENVTEST        ?= setup-envtest

# Version of the envtest control-plane binaries (etcd/kube-apiserver) to use
# for the envtest suite under test/envtest. Keep in sync with
# .github/workflows/ci.yaml's envtest job.
ENVTEST_K8S_VERSION ?= 1.31.0

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: ## Generate the manager ClusterRole and CRDs.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

.PHONY: fmt
fmt: ## Run golangci-lint formatters against code.
	$(GOLANGCI_LINT) fmt

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: deps
deps: ## Tidy and verify Go module dependencies.
	go mod tidy
	go mod verify

.PHONY: lint
lint: ## Run golangci-lint linter.
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: ## Run golangci-lint linter and perform fixes.
	$(GOLANGCI_LINT) run --fix

.PHONY: lint-config
lint-config: ## Verify golangci-lint linter configuration.
	$(GOLANGCI_LINT) config verify

UNIT_TEST_PACKAGES ?= ./api/... ./assets/... ./cmd/... ./internal/... ./pkg/... ./test/support/...

.PHONY: test
test: test-unit ## Run unit tests.

.PHONY: test-unit
test-unit: ## Run unit tests in the selected packages without a cluster.
	go test $(UNIT_TEST_PACKAGES) -coverprofile cover.out

.PHONY: test-envtest
test-envtest: manifests ## Run the envtest-based reconciler suite (no external cluster required).
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" \
		go test ./test/envtest/... -v -timeout 5m -failfast

.PHONY: test-integration-setup
test-integration-setup: manifests ## Prepare the cluster for integration tests.
	@context="$$($(KUBECTL) config current-context)"; \
	if [ "$$context" != "$(TEST_INTEGRATION_CONTEXT)" ]; then \
		echo "integration setup requires kube context $(TEST_INTEGRATION_CONTEXT) (current: $$context)" >&2; \
		exit 1; \
	fi
	$(KUSTOMIZE) build config/crd/bases | $(KUBECTL) apply -f -

.PHONY: test-integration-run
test-integration-run: ## Run integration tests against the current kubeconfig context's cluster.
	go test ./test/integration/... -v -timeout 5m -failfast

.PHONY: test-integration
test-integration: test-integration-setup test-integration-run ## Set up and run integration tests.

.PHONY: test-e2e-setup
test-e2e-setup: test-e2e-context-check ## Build/load the manager image and install the Helm chart into Kind.
	$(KUBECTL) get --raw=/readyz >/dev/null
	$(MAKE) test-e2e-teardown
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
		--set-string config.postgres-image="$(E2E_INTERNAL_POSTGRES_IMAGE)" "$$@"

.PHONY: test-e2e-run
test-e2e-run: test-e2e-context-check ## Verify the Helm-deployed manager reconciles DatabaseService, then clean up.
	$(KUBECTL) get --raw=/readyz >/dev/null
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
test-e2e-teardown: test-e2e-context-check ## Remove the e2e DatabaseService, Helm release, and namespace.
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
# Remove these direct CRD prerequisites when RHOAIENG-99478 installs them as module operands.
	$(KUBECTL) apply -f config/crd/bases/infrastructure.opendatahub.io_databaseproviders.yaml \
		-f config/crd/bases/infrastructure.opendatahub.io_databaseclaims.yaml \
		-f config/crd/bases/infrastructure.opendatahub.io_schemaclaims.yaml
	$(KUBECTL) wait --for=condition=Established --timeout=60s \
		crd/databaseproviders.infrastructure.opendatahub.io \
		crd/databaseclaims.infrastructure.opendatahub.io \
		crd/schemaclaims.infrastructure.opendatahub.io
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
E2E_CERT_MANAGER_PROFILE ?=
E2E_EXTERNAL_ADMIN_SECRET_NAMESPACE ?=
E2E_EXTERNAL_ADMIN_SECRET_NAME ?=
E2E_CONSUMER_IMAGE ?= postgres:16
E2E_INTERNAL_POSTGRES_IMAGE ?= quay.io/sclorg/postgresql-16-c9s@sha256:66566a27e08ec28d739c5fc54dcd47a4f0b505682d469589e9d493ae0b7bc9ab

.PHONY: container-load-kind
container-load-kind: ## Load $(IMG) into a Kind cluster. `kind load docker-image` doesn't work with the podman provider.
	tmp="$$(mktemp)"; trap 'rm -f "$$tmp" "$$tmp.tar"' EXIT; \
	$(CONTAINER_TOOL) save "$(IMG)" -o "$$tmp.tar"; \
	$(KIND) load image-archive "$$tmp.tar" --name "$(KIND_CLUSTER)"

##@ Helm

.PHONY: helm
helm: manifests generate ## Generate and lint the Helm chart from kustomize output via chartgen.
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
install: manifests ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( $(KUSTOMIZE) build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | $(KUBECTL) apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config.
	@out="$$( $(KUSTOMIZE) build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests ## Deploy controller to the K8s cluster specified in ~/.kube/config, via kustomize.
	cd config/manager && $(KUSTOMIZE) edit set image controller="$(IMG)"
	$(KUSTOMIZE) build config/default | $(KUBECTL) apply -f -

.PHONY: undeploy
undeploy: ## Undeploy controller from the K8s cluster specified in ~/.kube/config.
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

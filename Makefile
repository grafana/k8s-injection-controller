# Image URL to use all building/pushing image targets
IMG ?= ghcr.io/grafana/k8s-injection-controller:main
# Beyla image used by the demo targets. Override to point at a local
# development build (e.g. BEYLA_IMG=ghcr.io/me/beyla:dev).
BEYLA_IMG ?= grafana/beyla:main
# YEAR defines the year value used for substituting the YEAR placeholder in the boilerplate header.
YEAR ?= $(shell date +%Y)

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# Runs the e2e suite under test/e2e once per webhook cert strategy: cert-manager
# (a cluster with cert-manager) and self-signed (a cluster without it, using the
# controller's in-process cert rotator). Each run creates and destroys its own
# Kind cluster, so both the injection and metrics specs are exercised under both
# strategies.
# To keep the Kind cluster after the run (e.g. for debugging), set:
# - KIND_KEEP_CLUSTER=true
#
# The suite runs twice:
#   1. CERT_MODE=cert-manager — full suite (all 4 test files)
#   2. CERT_MODE=self-signed  — injection test only (e2e_injection_test.go)
#
# Only e2e_injection_test.go is cert-mode-sensitive (it directly asserts on
# cert-manager provisioning and CA bundle injection). The other three suites
# test application-level behaviour that is independent of how the webhook's
# TLS certificate is provisioned, so running them twice would double runtime
# for no additional coverage.
.PHONY: test-e2e
test-e2e: manifests generate fmt vet
	CERT_MODE=cert-manager go test -tags=e2e ./test/e2e/... -v -ginkgo.v -timeout 30m
	CERT_MODE=self-signed go test -tags=e2e ./test/e2e/... -v -ginkgo.v -ginkgo.focus-file="e2e_injection_test.go" -timeout 15m

.PHONY: helm-template-check
helm-template-check: ## Assert the chart renders correctly in cert-manager and self-signed modes.
	@set -e; set +o pipefail; CHART=charts/k8s-injection-controller; \
	echo "[auto, no cert-manager API] -> self-signed"; \
	helm template t $$CHART | grep -q "enable-cert-rotation" || { echo "FAIL: expected self-signed rotation"; exit 1; }; \
	helm template t $$CHART | grep -q "kind: Certificate" && { echo "FAIL: unexpected Certificate"; exit 1; } || true; \
	echo "[auto, cert-manager API present] -> cert-manager"; \
	helm template t $$CHART --api-versions cert-manager.io/v1 | grep -q "kind: Certificate" || { echo "FAIL: expected Certificate"; exit 1; }; \
	helm template t $$CHART --api-versions cert-manager.io/v1 | grep -q "enable-cert-rotation" && { echo "FAIL: unexpected rotation flag"; exit 1; } || true; \
	echo "[mode=cert-manager without API] -> install cert-manager via pre-install hook"; \
	helm template t $$CHART --set webhook.certManager.mode=cert-manager | grep -q "cert-manager-install" || { echo "FAIL: expected the cert-manager install hook"; exit 1; }; \
	helm template t $$CHART --set webhook.certManager.mode=cert-manager | grep -q "kind: Certificate" || { echo "FAIL: expected Certificate (post-install hook)"; exit 1; }; \
	echo "[mode=cert-manager with API] -> use existing cert-manager, no install hook"; \
	helm template t $$CHART --set webhook.certManager.mode=cert-manager --api-versions cert-manager.io/v1 | grep -q "cert-manager-install" && { echo "FAIL: unexpected install hook when cert-manager present"; exit 1; } || true; \
	echo "helm-template-check OK"

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager ./cmd

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name k8s-injection-controller-builder
	$(CONTAINER_TOOL) buildx use k8s-injection-controller-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm k8s-injection-controller-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/cert-manager > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

# Cert strategy used by `make deploy`/`make undeploy`. Override to deploy the
# self-signed assembly, e.g. `make deploy DEPLOY_CONFIG=config/self-signed`
# (the e2e suite does this to exercise both cert modes).
DEPLOY_CONFIG ?= config/cert-manager

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config. Override the cert strategy with DEPLOY_CONFIG=config/self-signed.
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build $(DEPLOY_CONFIG) | "$(KUBECTL)" apply -f -

.PHONY: yaml
yaml: manifests kustomize ## Render the deployable controller manifest (with a default mounted SDK config) to yaml/controller.yaml.
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	mkdir -p yaml
	"$(KUSTOMIZE)" build config/deploy > yaml/controller.yaml
	@echo "The controller yaml has been written to yaml/controller.yaml"

.PHONY: yaml-cert
yaml-cert: manifests kustomize ## Render the deployable controller manifest (with a default mounted SDK config) to yaml/controller.yaml.
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	mkdir -p yaml
	"$(KUSTOMIZE)" build config/deploy-manager > yaml/controller.yaml
	@echo "The controller yaml with cert-manager has been written to yaml/controller.yaml"

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion. Honors DEPLOY_CONFIG.
	"$(KUSTOMIZE)" build $(DEPLOY_CONFIG) | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: demo-deploy
demo-deploy: manifests kustomize ## Demo: deploy controller (via deploy-test) plus a Beyla DaemonSet wired to it and a sample workload.
	# 1. Deploy the controller with its sample SDK config.
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/test | "$(KUBECTL)" apply -f -
	# 1b. Wait for the controller to be serving. Its ConfigMap validating
	#     webhook (failurePolicy=Fail, scoped to the beyla-k8s-injector
	#     namespace) intercepts every ConfigMap write in that namespace,
	#     including Beyla's own beyla-config below. Applying beyla.yaml before
	#     the webhook server is up yields "connection refused".
	"$(KUBECTL)" -n beyla-k8s-injector rollout status deployment/beyla-k8s-injector-controller-manager --timeout=120s
	# 2. Deploy Beyla as a DaemonSet, with the webhook delegated to the
	#    beyla-k8s-injector-controller-manager. The per-node ConfigMap Beyla writes is what
	#    drives the controller in this demo.
	"$(KUBECTL)" apply -f examples/demo/beyla.yaml
	"$(KUBECTL)" -n beyla-k8s-injector set image daemonset/beyla beyla=$(BEYLA_IMG)
	# 3. Deploy a sample workload that matches Beyla's instrument criteria.
	"$(KUBECTL)" apply -f examples/demo/sample-app.yaml
	@echo
	@echo "Demo deployed. Useful checks:"
	@echo "  kubectl -n beyla-k8s-injector get pods"
	@echo "  kubectl -n beyla-k8s-injector get configmaps          # per-node state ConfigMaps Beyla just wrote"
	@echo "  kubectl -n beyla-k8s-injector logs deploy/beyla-k8s-injector-controller-manager"
	@echo "  kubectl -n demo get pods                      # workload pods the controller may bounce/inject"

.PHONY: demo-undeploy
demo-undeploy: kustomize ## Demo: tear down everything demo-deploy created.
	-"$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f examples/demo/sample-app.yaml
	-"$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f examples/demo/beyla.yaml
	-"$(KUSTOMIZE)" build config/test | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.20.1

#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?([0-9]+)\.([0-9]+).*/release-\1.\2/')

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.11.4
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
	@test -f .custom-gcl.yml && { \
		echo "Building custom golangci-lint with plugins..." && \
		$(GOLANGCI_LINT) custom --destination $(LOCALBIN) --name golangci-lint-custom && \
		mv -f $(LOCALBIN)/golangci-lint-custom $(GOLANGCI_LINT); \
	} || true

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef

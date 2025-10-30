# Image URL to use all building/pushing image targets
IMG ?= controller:latest

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
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen defaulter-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."
	$(DEFAULTER_GEN) ./internal/config --output-file=zz_generated.defaults.go

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# TODO(user): To use a different vendor for e2e tests, modify the setup under 'tests/e2e'.
# The default setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
# CertManager is installed by default; skip with:
# - CERT_MANAGER_INSTALL_SKIP=true
KIND_CLUSTER ?= dns-operator-test-e2e

.PHONY: setup-test-e2e
setup-test-e2e: ## Set up a Kind cluster for e2e tests if it does not exist
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@case "$$($(KIND) get clusters)" in \
		*"$(KIND_CLUSTER)"*) \
			echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation." ;; \
		*) \
			echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; \
			$(KIND) create cluster --name $(KIND_CLUSTER) ;; \
	esac


.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	$(GOLANGCI_LINT) config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

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
	- $(CONTAINER_TOOL) buildx create --name dns-operator-builder
	$(CONTAINER_TOOL) buildx use dns-operator-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm dns-operator-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( $(KUSTOMIZE) build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | $(KUBECTL) apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$( $(KUSTOMIZE) build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default | $(KUBECTL) apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/default | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: cert-manager
cert-manager: kustomize cmctl ## Install cert-manager into the cluster selected by CONTEXT and/or KUBECONFIG
	@FLAGS="$(if $(CONTEXT),--context $(CONTEXT),) $(if $(KUBECONFIG),--kubeconfig $(KUBECONFIG),)"; \
	echo "[cert-manager] applying to $$FLAGS"; \
	$(KUSTOMIZE) build --enable-helm config/tools/cert-manager \
	  | $(KUBECTL) $$FLAGS apply --server-side=true --force-conflicts -f -; \
	$(CMCTL) $$FLAGS check api --wait=5m

##@ Kind bootstrap

# Cluster names
DOWNSTREAM_CLUSTER_NAME ?= dns-downstream
UPSTREAM_CLUSTER_NAME ?= dns-upstream

# Image to load into kind clusters
KIND_IMAGE ?= $(IMG)

# Host to rewrite kubeconfig servers for in-cluster access (Docker Desktop/macOS default)
KIND_KUBECONFIG_HOST ?= host.docker.internal

.PHONY: kind-create
kind-create: ## Create a kind cluster with name CLUSTER
	@test -n "$(CLUSTER)" || { echo "CLUSTER is required, e.g. make kind-create CLUSTER=$(DOWNSTREAM_CLUSTER_NAME)"; exit 1; }
	@case "$$($(KIND) get clusters)" in \
		*"$(CLUSTER)"*) echo "Kind cluster '$(CLUSTER)' already exists. Skipping." ;; \
		*) echo "Creating Kind cluster '$(CLUSTER)'..."; $(KIND) create cluster --name $(CLUSTER) ;; \
	esac

.PHONY: kind-delete
kind-delete: ## Delete a kind cluster with name CLUSTER
	@test -n "$(CLUSTER)" || { echo "CLUSTER is required"; exit 1; }
	$(KIND) delete cluster --name $(CLUSTER)

.PHONY: kind-load-image
kind-load-image: ## Build and load manager image into kind cluster
	@test -n "$(CLUSTER)" || { echo "CLUSTER is required"; exit 1; }
	$(MAKE) docker-build IMG=$(KIND_IMAGE)
	$(KIND) load docker-image $(KIND_IMAGE) --name $(CLUSTER)

.PHONY: kustomize-apply
kustomize-apply: kustomize ## Apply a kustomize directory to a specific kubectl context
	@test -n "$(KUSTOMIZE_DIR)" || { echo "KUSTOMIZE_DIR is required"; exit 1; }
	@test -n "$(CONTEXT)" || { echo "CONTEXT is required"; exit 1; }
	$(KUSTOMIZE) build --load-restrictor LoadRestrictionsNone $(KUSTOMIZE_DIR) | $(KUBECTL) --context $(CONTEXT) apply -f -

.PHONY: export-kind-kubeconfig
export-kind-kubeconfig: ## Export kind kubeconfig for CLUSTER to OUT and rewrite server host
	@test -n "$(CLUSTER)" || { echo "CLUSTER is required"; exit 1; }
	@test -n "$(OUT)" || { echo "OUT is required"; exit 1; }
	@mkdir -p $(dir $(OUT))
	$(KIND) get kubeconfig --name $(CLUSTER) > $(OUT).raw
	# Replace server host with $(KIND_KUBECONFIG_HOST) to be reachable from other Docker containers
	sed -E "s#(server:[[:space:]]*https://)[^:]+(:[0-9]+)#\1$(KIND_KUBECONFIG_HOST)\2#g" $(OUT).raw > $(OUT)
	@if [ "$(KIND_KUBECONFIG_INSECURE)" = "true" ]; then \
		echo "Rewriting kubeconfig to skip TLS verify (dev only)"; \
		sed -E "s#^([[:space:]]*)certificate-authority-data:.*#\1insecure-skip-tls-verify: true#" $(OUT) > $(OUT).tmp; \
		mv $(OUT).tmp $(OUT); \
	fi
	@rm -f $(OUT).raw

.PHONY: export-kind-kubeconfig-raw
export-kind-kubeconfig-raw: ## Export kind kubeconfig for CLUSTER to OUT without rewriting
	@test -n "$(CLUSTER)" || { echo "CLUSTER is required"; exit 1; }
	@test -n "$(OUT)" || { echo "OUT is required"; exit 1; }
	@mkdir -p $(dir $(OUT))
	$(KIND) get kubeconfig --name $(CLUSTER) > $(OUT)

.PHONY: secret-from-file
secret-from-file: ## Create or update a secret from a file in NAMESPACE on CONTEXT
	@test -n "$(NAMESPACE)" || { echo "NAMESPACE is required"; exit 1; }
	@test -n "$(NAME)" || { echo "NAME is required"; exit 1; }
	@test -n "$(KEY)" || { echo "KEY is required"; exit 1; }
	@test -n "$(FILE)" || { echo "FILE is required"; exit 1; }
	@test -n "$(CONTEXT)" || { echo "CONTEXT is required"; exit 1; }
	$(KUBECTL) --context $(CONTEXT) -n $(NAMESPACE) create secret generic $(NAME) --from-file=$(KEY)=$(FILE) --dry-run=client -o yaml | $(KUBECTL) --context $(CONTEXT) apply -f -

.PHONY: bootstrap-downstream
bootstrap-downstream: ## Create kind downstream and deploy agent with embedded PowerDNS
	CLUSTER=$(DOWNSTREAM_CLUSTER_NAME) $(MAKE) kind-create
	CLUSTER=$(DOWNSTREAM_CLUSTER_NAME) $(MAKE) kind-load-image
	CONTEXT=kind-$(DOWNSTREAM_CLUSTER_NAME) $(MAKE) cert-manager
	CONTEXT=kind-$(DOWNSTREAM_CLUSTER_NAME) KUSTOMIZE_DIR=config/overlays/agent-powerdns $(MAKE) kustomize-apply
	# Export external kubeconfig for downstream cluster (reachable from host/other containers)
	CLUSTER=$(DOWNSTREAM_CLUSTER_NAME) OUT=dev/kind.downstream.kubeconfig $(MAKE) export-kind-kubeconfig-raw

.PHONY: bootstrap-upstream
bootstrap-upstream: ## Create kind upstream and deploy replicator pointing to downstream
	@test -n "$(DOWNSTREAM_KUBECONFIG)" || { echo "DOWNSTREAM_KUBECONFIG is required. Generate with: make export-downstream-kubeconfig OUT=dev/downstream.kubeconfig"; exit 1; }
	CLUSTER=$(UPSTREAM_CLUSTER_NAME) $(MAKE) kind-create
	CLUSTER=$(UPSTREAM_CLUSTER_NAME) $(MAKE) kind-load-image
	CONTEXT=kind-$(UPSTREAM_CLUSTER_NAME) $(MAKE) cert-manager
	# Ensure namespace exists for secret
	$(KUBECTL) --context kind-$(UPSTREAM_CLUSTER_NAME) create namespace dns-replicator-system --dry-run=client -o yaml | $(KUBECTL) --context kind-$(UPSTREAM_CLUSTER_NAME) apply -f -
	# Create secret with downstream kubeconfig in upstream cluster
	CONTEXT=kind-$(UPSTREAM_CLUSTER_NAME) NAMESPACE=dns-replicator-system NAME=downstream-kubeconfig KEY=kubeconfig FILE=$(DOWNSTREAM_KUBECONFIG) $(MAKE) secret-from-file
	# Deploy replicator overlay
	CONTEXT=kind-$(UPSTREAM_CLUSTER_NAME) KUSTOMIZE_DIR=config/overlays/replicator $(MAKE) kustomize-apply
	# Export external kubeconfig for upstream cluster (reachable from host/other containers)
	CLUSTER=$(UPSTREAM_CLUSTER_NAME) OUT=dev/kind.upstream.kubeconfig $(MAKE) export-kind-kubeconfig-raw

.PHONY: export-downstream-kubeconfig
export-downstream-kubeconfig: ## Export downstream kubeconfig rewritten for in-cluster usage by upstream
	@test -n "$(OUT)" || { echo "OUT is required"; exit 1; }
	CLUSTER=$(DOWNSTREAM_CLUSTER_NAME) OUT=$(OUT) $(MAKE) export-kind-kubeconfig

## End-to-end bootstrap (downstream → export kubeconfig → upstream)
E2E_DOWNSTREAM_KUBECONFIG ?= dev/downstream.kubeconfig
E2E_INSECURE ?= true
.PHONY: bootstrap-e2e
bootstrap-e2e: ## Bootstrap downstream, export kubeconfig, then bootstrap upstream
	$(MAKE) bootstrap-downstream IMG=$(IMG)
	OUT=$(E2E_DOWNSTREAM_KUBECONFIG) KIND_KUBECONFIG_INSECURE=$(E2E_INSECURE) $(MAKE) export-downstream-kubeconfig
	DOWNSTREAM_KUBECONFIG=$(E2E_DOWNSTREAM_KUBECONFIG) IMG=$(IMG) $(MAKE) bootstrap-upstream
	@echo "E2E bootstrap complete. Upstream: $(UPSTREAM_CLUSTER_NAME), Downstream: $(DOWNSTREAM_CLUSTER_NAME)."
	@echo "Downstream kubeconfig: $(E2E_DOWNSTREAM_KUBECONFIG)"
	@echo "External kubeconfigs: dev/kind.downstream.kubeconfig, dev/kind.upstream.kubeconfig"

##@ DNS debugging

# Defaults for DNS debug target
DNS_DEBUG_CONTEXT ?= kind-dns-downstream
DNS_DEBUG_NAMESPACE ?= dns-agent-system
DNS_DEBUG_ZONE ?= example.com
DNS_DEBUG_HOST ?= www

.PHONY: dns-debug
dns-debug: ## Launch a DNS tools pod and query PDNS SOA/NS/A for $(DNS_DEBUG_ZONE)
	@echo "[dns-debug] Ensuring dnstools pod exists in $(DNS_DEBUG_NAMESPACE)"
	@kubectl --context $(DNS_DEBUG_CONTEXT) -n $(DNS_DEBUG_NAMESPACE) get pod dnstools >/dev/null 2>&1 || \
		kubectl --context $(DNS_DEBUG_CONTEXT) -n $(DNS_DEBUG_NAMESPACE) run dnstools --image=infoblox/dnstools:latest --restart=Never --command -- sh -c 'sleep 3600'
	@kubectl --context $(DNS_DEBUG_CONTEXT) -n $(DNS_DEBUG_NAMESPACE) wait --for=condition=Ready pod/dnstools --timeout=90s
	@echo "[dns-debug] Querying PDNS Service pdns-auth.$(DNS_DEBUG_NAMESPACE).svc.cluster.local"
	@kubectl --context $(DNS_DEBUG_CONTEXT) -n $(DNS_DEBUG_NAMESPACE) exec dnstools -- \
		sh -lc 'echo SOA:; dig +time=2 +tries=1 +short @pdns-auth.$(DNS_DEBUG_NAMESPACE).svc.cluster.local $(DNS_DEBUG_ZONE) SOA; \
		echo NS:;  dig +time=2 +tries=1 +short @pdns-auth.$(DNS_DEBUG_NAMESPACE).svc.cluster.local $(DNS_DEBUG_ZONE) NS; \
		echo A $(DNS_DEBUG_HOST).$(DNS_DEBUG_ZONE):; dig +time=2 +tries=1 +short @pdns-auth.$(DNS_DEBUG_NAMESPACE).svc.cluster.local $(DNS_DEBUG_HOST).$(DNS_DEBUG_ZONE) A'

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
DEFAULTER_GEN ?= $(LOCALBIN)/defaulter-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
CHAINSAW ?= $(LOCALBIN)/chainsaw
CMCTL ?= $(LOCALBIN)/cmctl

## Tool Versions
KUSTOMIZE_VERSION ?= v5.7.1
CONTROLLER_TOOLS_VERSION ?= v0.19.0
DEFAULTER_GEN_VERSION ?= v0.32.3

#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell go list -m -f "{{ .Version }}" sigs.k8s.io/controller-runtime | awk -F'[v.]' '{printf "release-%d.%d", $$2, $$3}')
#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell go list -m -f "{{ .Version }}" k8s.io/api | awk -F'[v.]' '{printf "1.%d", $$3}')
GOLANGCI_LINT_VERSION ?= v2.4.0
CHAINSAW_VERSION ?= v0.2.13
CERTMANAGER_VERSION ?= 1.17.1
CMCTL_VERSION ?= v2.1.1

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: defaulter-gen
defaulter-gen: $(DEFAULTER_GEN) ## Download defaulter-gen locally if necessary.
$(DEFAULTER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(DEFAULTER_GEN),k8s.io/code-generator/cmd/defaulter-gen,$(DEFAULTER_GEN_VERSION))


.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path || { \
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

.PHONY: cmctl
cmctl: ## Find or download cmctl
	$(call go-install-tool,$(CMCTL),github.com/cert-manager/cmctl/v2,$(CMCTL_VERSION))

.PHONY: chainsaw
chainsaw: ## Find or download chainsaw
	$(call go-install-tool,$(CHAINSAW),github.com/kyverno/chainsaw,$(CHAINSAW_VERSION))

.PHONY: chainsaw-test
chainsaw-test: chainsaw chainsaw-prepare-kubeconfigs ## Run Chainsaw tests (requires dev/kind.*.kubeconfig to exist)
	@test -f dev/kind.upstream.kubeconfig || { echo "Missing dev/kind.upstream.kubeconfig. Bootstrap clusters first."; exit 1; }
	@test -f dev/kind.downstream.kubeconfig || { echo "Missing dev/kind.downstream.kubeconfig. Bootstrap clusters first."; exit 1; }
	cd test/e2e && $(CHAINSAW) test .

.PHONY: chainsaw-prepare-kubeconfigs
chainsaw-prepare-kubeconfigs: ## Copy dev kind kubeconfigs into test/e2e for stable relative resolution
	@test -f dev/kind.upstream.kubeconfig || { echo "Missing dev/kind.upstream.kubeconfig. Bootstrap clusters first."; exit 1; }
	@test -f dev/kind.downstream.kubeconfig || { echo "Missing dev/kind.downstream.kubeconfig. Bootstrap clusters first."; exit 1; }
	cp dev/kind.upstream.kubeconfig test/e2e/kubeconfig-upstream
	cp dev/kind.downstream.kubeconfig test/e2e/kubeconfig-downstream

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $$(realpath $(1)-$(3)) $(1)
endef

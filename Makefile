# BAIRAVS Orch Engine — top-level Makefile
# Orchestrates build, test, lint across the Go multi-module workspace.

.DEFAULT_GOAL := help

# -------- Config --------

GO           ?= go
GOLANGCI     ?= golangci-lint
MODULES      := cli operator
BIN_DIR      := $(CURDIR)/bin

# Version resolution (cascading fallback so VERSION is always SemVer-shaped):
#   1. If a git tag is reachable from HEAD:   v0.2.0  (or v0.2.0-dirty if uncommitted)
#   2. Else if we're inside a git repo:       v0.0.0-dev+<short-sha>[-dirty]
#   3. Else (no git available):                v0.0.0-dev
GIT_TAG      := $(shell git describe --tags --dirty 2>/dev/null)
GIT_SHA      := $(shell git rev-parse --short HEAD 2>/dev/null)
GIT_DIRTY    := $(shell git status --porcelain 2>/dev/null | head -1 | awk '{print "-dirty"}')
VERSION      ?= $(if $(GIT_TAG),$(GIT_TAG),$(if $(GIT_SHA),v0.0.0-dev+$(GIT_SHA)$(GIT_DIRTY),v0.0.0-dev))
LDFLAGS      := -ldflags "-X main.Version=$(VERSION)"

# -------- Help --------

.PHONY: help
help: ## Show this help message
	@echo "BAIRAVS Orch Engine — Makefile targets"
	@echo ""
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

# -------- Setup --------

.PHONY: deps
deps: ## Install Go module dependencies for every initialized module
	@for m in $(MODULES); do \
		if [ -f "$$m/go.mod" ]; then \
			echo "==> go mod tidy in $$m"; \
			(cd $$m && $(GO) mod tidy) || exit 1; \
		else \
			echo "==> skipping $$m (no go.mod yet — module not yet scaffolded)"; \
		fi; \
	done

.PHONY: tools
tools: ## Install dev tools (golangci-lint, kubebuilder, etc.)
	@echo "==> installing golangci-lint v2"
	@if command -v brew >/dev/null 2>&1; then \
		brew install golangci-lint; \
	else \
		curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh | \
			sh -s -- -b $$($(GO) env GOPATH)/bin v2.1.6; \
	fi
	@echo ""
	@echo "Install kubebuilder separately: https://book.kubebuilder.io/quick-start.html"

# -------- Build --------

.PHONY: build
build: $(addprefix build-,$(MODULES)) ## Build all modules

build-cli: ## Build the bairav CLI binary
	@mkdir -p $(BIN_DIR)
	cd cli && $(GO) build $(LDFLAGS) -o $(BIN_DIR)/bairav ./

build-operator: ## Build the operator binary
	@mkdir -p $(BIN_DIR)
	cd operator && $(GO) build $(LDFLAGS) -o $(BIN_DIR)/bairav-operator ./cmd


# -------- Test --------

.PHONY: test
test: ## Run unit tests across all modules
	@for m in $(MODULES); do \
		if [ -f "$$m/go.mod" ]; then \
			echo "==> testing $$m"; \
			(cd $$m && $(GO) test ./... -race -coverprofile=coverage.txt -covermode=atomic) || exit 1; \
		else \
			echo "==> skipping $$m (no go.mod yet)"; \
		fi; \
	done

.PHONY: test-e2e
test-e2e: ## Run the end-to-end smoke suite against docker (and k8s if the CRDs are installed)
	bash scripts/smoke-day28.sh

# -------- Lint --------

.PHONY: lint
lint: ## Run golangci-lint across all modules
	@command -v $(GOLANGCI) >/dev/null 2>&1 || { \
		echo "Error: $(GOLANGCI) not found in PATH."; \
		echo ""; \
		echo "Install with one of:"; \
		echo "  brew install golangci-lint        # macOS, fastest"; \
		echo "  make tools                        # auto-detects brew, else curls v2.1.6"; \
		echo "  See https://golangci-lint.run/welcome/install/ for other platforms."; \
		exit 1; \
	}
	@for m in $(MODULES); do \
		if [ -f "$$m/go.mod" ]; then \
			echo "==> linting $$m"; \
			(cd $$m && $(GOLANGCI) run ./...) || exit 1; \
		else \
			echo "==> skipping $$m (no go.mod yet)"; \
		fi; \
	done

.PHONY: fmt
fmt: ## Format Go code
	@for m in $(MODULES); do \
		if [ -f "$$m/go.mod" ]; then \
			echo "==> formatting $$m"; \
			(cd $$m && $(GO) fmt ./...); \
		fi; \
	done

# -------- CI (local pre-flight) --------

.PHONY: ci
ci: ## Run the same checks CI runs (gofmt, tidy, vet, lint, test, build)
	@./scripts/ci.sh

.PHONY: rebuild
rebuild: ## Shortcut: ci + build-cli — green checks then a fresh ./bin/bairav
	@./scripts/ci.sh
	@$(MAKE) --no-print-directory build-cli
	@echo ""
	@echo "Fresh binary: $(BIN_DIR)/bairav"

.PHONY: rebuild-all
rebuild-all: ## ci + CLI + every image (operator, dlp, mcp-echo) — no stale images
	@$(MAKE) --no-print-directory rebuild
	@$(MAKE) --no-print-directory image-operator
	@$(MAKE) --no-print-directory image-dlp
	@$(MAKE) --no-print-directory image-mcp-echo
	@echo ""
	@echo "All images rebuilt at $(VERSION). Restart the operator to pick it up:"
	@echo "  kubectl -n bairav-system rollout restart deploy/bairav-operator"

# -------- Container images --------

K3D_CLUSTER ?= bairavs

.PHONY: image-serving
image-serving: ## Build the bairav/serving:laptop image and import into k3d
	docker build -t bairav/serving:laptop -f images/serving/Dockerfile images/serving/
	@if command -v k3d >/dev/null 2>&1 && k3d cluster list | grep -q "^$(K3D_CLUSTER) "; then \
		echo "==> importing into k3d cluster $(K3D_CLUSTER)"; \
		k3d image import bairav/serving:laptop -c $(K3D_CLUSTER); \
	else \
		echo "(skip k3d import: cluster '$(K3D_CLUSTER)' not found)"; \
	fi

.PHONY: image-operator
image-operator: ## Build the bairav/operator:laptop image and import into k3d
	docker build -t bairav/operator:laptop -f operator/Dockerfile operator/
	@if command -v k3d >/dev/null 2>&1 && k3d cluster list | grep -q "^$(K3D_CLUSTER) "; then \
		echo "==> importing into k3d cluster $(K3D_CLUSTER)"; \
		k3d image import bairav/operator:laptop -c $(K3D_CLUSTER); \
	else \
		echo "(skip k3d import: cluster '$(K3D_CLUSTER)' not found)"; \
	fi

.PHONY: image-dlp
image-dlp: ## Build the bairav/dlp:laptop image and import into k3d
	# Build context is the repo root (not images/dlp/) so the Dockerfile
	# can COPY go.work + cli/ + operator/. Distroless static base keeps
	# the runtime image around ~50MB — one static Go binary + nothing else.
	docker build --build-arg VERSION=$(VERSION) -t bairav/dlp:laptop -f images/dlp/Dockerfile .
	@if command -v k3d >/dev/null 2>&1 && k3d cluster list | grep -q "^$(K3D_CLUSTER) "; then \
		echo "==> importing into k3d cluster $(K3D_CLUSTER)"; \
		k3d image import bairav/dlp:laptop -c $(K3D_CLUSTER); \
	else \
		echo "(skip k3d import: cluster '$(K3D_CLUSTER)' not found)"; \
	fi

.PHONY: image-mcp-echo
image-mcp-echo: ## Build the bairav/mcp-echo:v1 reference Skill image and import into k3d
	docker build -t bairav/mcp-echo:v1 examples/mcp-echo
	@if command -v k3d >/dev/null 2>&1 && k3d cluster list | grep -q "^$(K3D_CLUSTER) "; then \
		k3d image import bairav/mcp-echo:v1 -c $(K3D_CLUSTER); \
	else \
		echo "(skip k3d import: cluster '$(K3D_CLUSTER)' not found)"; \
	fi

.PHONY: install-crds
install-crds: ## Apply BAIRAVS CRDs to the current K8s context
	kubectl apply -f operator/config/crd/bases/

.PHONY: install-operator
install-operator: image-operator install-crds ## Install operator + CRDs into the current K8s context
	# The bairav-system namespace is declared inside manager.yaml, but the
	# RBAC manifests target it by name — so on a fresh cluster, applying
	# rbac.yaml before manager.yaml fails with `namespaces "bairav-system"
	# not found`. Create the namespace explicitly first (idempotent), then
	# apply RBAC, then the manager.
	kubectl create namespace bairav-system --dry-run=client -o yaml | kubectl apply -f -
	kubectl apply -f operator/config/rbac/
	kubectl apply -f operator/config/manager/
	@echo ""
	@echo "Operator installed. Watch it come up:"
	@echo "  kubectl -n bairav-system get pods -w"

.PHONY: uninstall-operator
uninstall-operator: ## Tear down operator + RBAC + CRDs
	-kubectl delete -f operator/config/manager/
	-kubectl delete -f operator/config/rbac/
	-kubectl delete -f operator/config/crd/bases/

# -------- Dev --------
#
# `make dev` is the new-contributor onboarding loop. It is idempotent:
# running it on a clean Mac brings up the full K8s path (k3d cluster +
# images imported + operator installed); running it again on a working
# setup is a no-op aside from re-tailing the operator logs.

.PHONY: dev
dev: dev-k8s ## Alias for `make dev-k8s` (the default dev loop).

.PHONY: dev-k8s
dev-k8s: ## Bootstrap a full local K8s dev environment (k3d + images + operator) and tail logs.
	@command -v docker  >/dev/null 2>&1 || { echo "dev-k8s: docker not found in PATH"; exit 1; }
	@command -v k3d     >/dev/null 2>&1 || { echo "dev-k8s: k3d not found in PATH (install: https://k3d.io)"; exit 1; }
	@command -v kubectl >/dev/null 2>&1 || { echo "dev-k8s: kubectl not found in PATH"; exit 1; }
	@if k3d cluster list | grep -q "^$(K3D_CLUSTER) "; then \
		echo "==> k3d cluster '$(K3D_CLUSTER)' already exists"; \
	else \
		echo "==> creating k3d cluster '$(K3D_CLUSTER)'"; \
		k3d cluster create $(K3D_CLUSTER); \
	fi
	@echo ""
	@echo "==> building + importing serving image"
	$(MAKE) image-serving
	@echo ""
	@echo "==> building + importing operator image"
	$(MAKE) image-operator
	@echo ""
	@echo "==> installing CRDs + operator"
	$(MAKE) install-crds
	# Namespace must exist before RBAC (see install-operator for the why).
	kubectl create namespace bairav-system --dry-run=client -o yaml | kubectl apply -f -
	kubectl apply -f operator/config/rbac/
	kubectl apply -f operator/config/manager/
	@echo ""
	@echo "==> waiting for operator pod (timeout 120s)"
	@kubectl -n bairav-system wait --for=condition=Available --timeout=120s deployment/bairav-operator || true
	@echo ""
	@echo "Cluster ready. Deploy a model:"
	@echo "  bin/bairav model deploy Qwen/Qwen2.5-0.5B-Instruct-GGUF --target k8s"
	@echo ""
	@echo "==> tailing operator logs (Ctrl-C to stop; logs keep running)"
	@kubectl -n bairav-system logs -f deployment/bairav-operator || true

.PHONY: dev-down
dev-down: ## Tear down the k3d dev cluster entirely.
	@if k3d cluster list | grep -q "^$(K3D_CLUSTER) "; then \
		echo "==> deleting k3d cluster '$(K3D_CLUSTER)'"; \
		k3d cluster delete $(K3D_CLUSTER); \
	else \
		echo "(no cluster '$(K3D_CLUSTER)' to delete)"; \
	fi

# -------- Cleanup --------

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR)
	rm -f coverage.txt coverage.html
	@for m in $(MODULES); do \
		rm -f $$m/coverage.txt $$m/coverage.html; \
	done

# -------- Versioning --------

.PHONY: version
version: ## Print the current version tag
	@echo $(VERSION)

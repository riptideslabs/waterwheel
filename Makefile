SHELL := /bin/bash
.DEFAULT_GOAL := help

CONTROLLER_GEN := go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.23.0
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0
BINS := waterwheel-agent waterwheel-controller waterwheel-proxy
CHART_VALUES := --set trustDomain=waterwheel.test --set issuer=https://oidc.waterwheel.test

.PHONY: help generate check-generate build test vet lint chart shellcheck check \
	vm cluster images deploy dev api-check e2e e2e-setup e2e-reset e2e-trust demo down vm-delete site site-build

help: ## List targets
	@grep -E '^[a-z0-9-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

# --- code -------------------------------------------------------------------

generate: ## Generate deep copies and the CRDs (charts/waterwheel/crds)
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths=./api/...
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:dir=charts/waterwheel/crds

check-generate: generate ## Fail if generated files aren't committed
	@test -z "$$(git status --porcelain api charts/waterwheel/crds)" || \
		{ git status --short api charts/waterwheel/crds; echo "run make generate and commit the result"; exit 1; }

build: ## Build the binaries into bin/
	@for b in $(BINS); do go build -o bin/$$b ./cmd/$$b || exit 1; done

test: ## Run the unit tests
	go test -race -count=1 ./...

vet: ## go vet
	go vet ./...

lint: vet ## go vet and golangci-lint
	$(GOLANGCI_LINT) run ./...

chart: ## helm lint, and render the chart for each identity source
	helm lint charts/waterwheel $(CHART_VALUES)
	helm template waterwheel charts/waterwheel >/dev/null
	helm template waterwheel charts/waterwheel $(CHART_VALUES) --set identity.source=spire >/dev/null
	helm template waterwheel charts/waterwheel $(CHART_VALUES) --set identity.source=waterwheel \
		--set issuer=https://oidc.example.com --set oidc.ingress.enabled=true --set oidc.ingress.host=oidc.example.com >/dev/null

shellcheck: ## shellcheck the scripts
	shellcheck -x -P SCRIPTDIR hack/*.sh test/e2e/*.sh test/e2e/steps/*.sh

site: ## Serve the project page (site/) at http://localhost:4321
	cd site && npm install && npm run dev

site-build: ## Build the project page into site/dist
	cd site && npm ci && npm run build

check: lint test chart shellcheck ## Everything CI runs, except check-generate

# --- kind -------------------------------------------------------------------

vm: ## Create/start the Lima VM waterwheel-dev (LIMA_VM= to use local Docker)
	@hack/vm.sh

cluster: ## Create the kind cluster "waterwheel" and .state/kubeconfig
	@hack/cluster.sh

images: ## Build the images and load them into kind
	@hack/images.sh

deploy: ## Install or upgrade Waterwheel in the kind cluster (IDENTITY_SOURCE=spire installs SPIRE too)
	@hack/deploy.sh

dev: vm cluster images deploy ## Everything: VM, cluster, images, Waterwheel

api-check: ## Dry-run the API samples in test/api against the cluster
	@hack/api-check.sh

e2e: vm cluster images e2e-setup e2e-reset ## The KCD demo's five steps, checked (E2E_PROVIDER=fake|anthropic, E2E_IDENTITY=serviceAccountToken|spire)
	@test/e2e/steps/all.sh

e2e-setup: ## Fake provider, test images, DNS, and Waterwheel with the e2e CA
	@test/e2e/setup.sh

e2e-reset: ## Back to the state before step 1
	@test/e2e/reset.sh
e2e-trust: ## CA trust in common base images, after make e2e (E2E_IDENTITY=serviceAccountToken)
	@test/e2e/trust.sh

demo: ## The five steps, waiting for a key before each command
	@PAUSE=1 test/e2e/steps/all.sh

down: ## Delete the kind cluster
	@source hack/lib.sh && vm "$$KIND" delete cluster --name "$$CLUSTER_NAME"

vm-delete: ## Delete the Lima VM waterwheel-dev
	limactl delete --force waterwheel-dev

## Inject the app version into operator.Version
LDFLAGS ?= -ldflags=-X=sigs.k8s.io/karpenter/pkg/operator.Version=$(shell git describe --tags --always | cut -d"v" -f2)

GOFLAGS ?= $(LDFLAGS)
WITH_GOFLAGS = GOFLAGS="$(GOFLAGS)"

## Extra helm options
CLUSTER_ENDPOINT ?= $(shell kubectl config view --minify -o jsonpath='{.clusters[].cluster.server}')
# CR for local builds of Karpenter
KARPENTER_NAMESPACE ?= karpenter-system
KARPENTER_VERSION ?= $(shell git tag --sort=committerdate | tail -1 | cut -d"v" -f2)
# KO_DOCKER_REPO ?= ${AWS_ACCOUNT_ID}.dkr.ecr.${AWS_DEFAULT_REGION}.amazonaws.com/dev
KO_DOCKER_REPO ?= ko.local
KOCACHE ?= ~/.ko

# GCP Cluster Context
# PROJECT_ID must be set explicitly — no default (project IDs are always personal/team-specific)
CLUSTER_NAME ?= karpenter-provider-gcp
REGION ?= us-central1

# Common Directories
MOD_DIRS = $(shell find . -path "./website" -prune -o -name go.mod -type f -print | xargs dirname)
KARPENTER_CORE_DIR = $(shell go list -m -f '{{ .Dir }}' sigs.k8s.io/karpenter)

help: ## Display help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

presubmit: verify-codegen verify chart-lint ut-test ## Run all steps in the developer loop

toolchain: ## Install developer toolchain
	./hack/toolchain.sh

run: ## Run Karpenter controller binary against your local cluster
	SYSTEM_NAMESPACE=${KARPENTER_NAMESPACE} \
		KUBERNETES_MIN_VERSION="v1.26.0" \
		DISABLE_LEADER_ELECTION=true \
		CLUSTER_NAME=${CLUSTER_NAME} \
		PROJECT_ID=${PROJECT_ID} \
		CLUSTER_LOCATION=${REGION} \
		INTERRUPTION_QUEUE=${CLUSTER_NAME} \
		FEATURE_GATES="SpotToSpotConsolidation=true,NodeOverlay=true" \
		go run ./cmd/controller/main.go

update: tidy download ## Update go files header, CRD and generated code
	hack/boilerplate.sh
	hack/update-generated.sh
	hack/docs-generate.sh
	helm-docs -c charts/karpenter
	helm-docs -c charts/karpenter-crd

verify-codegen: update ## Verify generated code is up to date
	git diff --exit-code || (echo "Generated files are out of date — run 'make update' and commit the changes" && exit 1)

chart-lint: ## Lint the Helm charts (validates values.schema.json and templates)
	helm lint charts/karpenter/
	helm lint charts/karpenter-crd/

verify: ## Verify code. Includes linting, formatting, etc
	golangci-lint run --new-from-rev=origin/main --timeout=20m
	git diff --exit-code || (echo "golangci-lint reformatted files above — stage and commit them" && exit 1)

DOCS_FILES = $(shell find docs -name "*.md" ! -path "docs/reference/*")

docs-lint: ## Check docs markdown formatting (excludes generated reference docs)
	mdox fmt --check $(DOCS_FILES)

docs-fix: ## Auto-fix docs markdown formatting
	mdox fmt $(DOCS_FILES)

image: ## Build the Karpenter controller images using ko build
	$(eval CONTROLLER_IMG=$(shell $(WITH_GOFLAGS) KOCACHE=$(KOCACHE) KO_DOCKER_REPO="$(KO_DOCKER_REPO)" ko build --bare github.com/cloudpilot-ai/karpenter-provider-gcp/cmd/controller))
	$(eval IMG_REPOSITORY=$(shell echo $(CONTROLLER_IMG) | cut -d "@" -f 1 | cut -d ":" -f 1))
	$(eval IMG_TAG=$(shell echo $(CONTROLLER_IMG) | cut -d "@" -f 1 | cut -d ":" -f 2 -s))

apply: image ## Deploy the controller from the current state of your git repository into your ~/.kube/config cluster
	helm upgrade --install karpenter charts/karpenter \
		--create-namespace \
		--namespace ${KARPENTER_NAMESPACE} \
		$(HELM_OPTS) \
		--set logLevel=debug \
		--set controller.image.repository=$(IMG_REPOSITORY) \
		--set controller.image.tag=$(IMG_TAG) \

delete: ## Delete the controller from your kubeconfig cluster
	helm uninstall karpenter --namespace ${KARPENTER_NAMESPACE}
	kubectl delete ns ${KARPENTER_NAMESPACE}

ut-test: ## Run unit tests
	go test ./pkg/... \
		-cover -coverprofile=coverage.out -outputdir=.

# E2E configuration — names are derived from E2E_PREFIX to stay consistent
# between the setup script and the test binary.
# E2E_PROJECT_ID, E2E_SA_PATH, and E2E_LOCATION must be set explicitly — no defaults.
E2E_PROJECT_ID   ?=
E2E_SA_PATH      ?=
E2E_LOCATION     ?=
E2E_PREFIX       ?= karpenter-e2e
E2E_REGION       ?= us-central1
E2E_CLUSTER_NAME ?= $(E2E_PREFIX)-cluster
E2E_PODS_RANGE   ?= $(E2E_PREFIX)-pods

e2e-setup: require-e2e-vars ## Create (or reuse) the e2e GKE cluster and supporting GCP infra
	GOOGLE_APPLICATION_CREDENTIALS=$(E2E_SA_PATH) \
	E2E_PROJECT_ID=$(E2E_PROJECT_ID) \
	E2E_PREFIX=$(E2E_PREFIX) \
	E2E_REGION=$(E2E_REGION) \
	E2E_LOCATION=$(E2E_LOCATION) \
	./hack/e2e-setup.sh

e2e-deploy: require-e2e-vars ## Build image and (re)deploy karpenter onto an existing e2e cluster
	GOOGLE_APPLICATION_CREDENTIALS=$(E2E_SA_PATH) \
	E2E_PROJECT_ID=$(E2E_PROJECT_ID) \
	E2E_PREFIX=$(E2E_PREFIX) \
	E2E_REGION=$(E2E_REGION) \
	E2E_LOCATION=$(E2E_LOCATION) \
	./hack/e2e-deploy.sh

require-e2e-vars: ## Fail fast if required e2e variables are not set
	@test -n "$(E2E_PROJECT_ID)"  || (echo "ERROR: E2E_PROJECT_ID is not set"  >&2 && exit 1)
	@test -n "$(E2E_SA_PATH)"     || (echo "ERROR: E2E_SA_PATH is not set"     >&2 && exit 1)
	@test -n "$(E2E_LOCATION)"    || (echo "ERROR: E2E_LOCATION is not set"    >&2 && exit 1)

GINKGO_PROCS ?= 4
e2e-tests: require-e2e-vars ## Run all e2e test suites in parallel (GINKGO_PROCS=N, default 4)
	GOOGLE_APPLICATION_CREDENTIALS=$(abspath $(E2E_SA_PATH)) \
	PROJECT_ID=$(E2E_PROJECT_ID) \
	CLUSTER_NAME=$(E2E_CLUSTER_NAME) \
	CLUSTER_LOCATION=$(E2E_LOCATION) \
	PODS_RANGE_NAME=$(E2E_PODS_RANGE) \
	go run github.com/onsi/ginkgo/v2/ginkgo --procs=$(GINKGO_PROCS) --timeout=2h -v ./test/suites/...

FOCUS ?=
SUITE ?=
e2e-test: require-e2e-vars ## Run a single e2e spec (FOCUS="<substring>", optionally SUITE=<suite-dir>)
	GOOGLE_APPLICATION_CREDENTIALS=$(abspath $(E2E_SA_PATH)) \
	PROJECT_ID=$(E2E_PROJECT_ID) \
	CLUSTER_NAME=$(E2E_CLUSTER_NAME) \
	CLUSTER_LOCATION=$(E2E_LOCATION) \
	PODS_RANGE_NAME=$(E2E_PODS_RANGE) \
	go test -count 1 -timeout 30m -v \
	$(if $(SUITE),./test/suites/$(SUITE)/,./test/suites/...) \
	-args -ginkgo.focus="$(FOCUS)" -ginkgo.v

e2e-teardown: ## Delete the e2e GKE cluster and all supporting GCP infra
	GOOGLE_APPLICATION_CREDENTIALS=$(E2E_SA_PATH) \
	E2E_PREFIX=$(E2E_PREFIX) \
	E2E_REGION=$(E2E_REGION) \
	E2E_LOCATION=$(E2E_LOCATION) \
	./hack/e2e-teardown.sh

e2e-check-clean: ## Report any orphaned e2e GCP resources (does not delete)
	GOOGLE_APPLICATION_CREDENTIALS=$(E2E_SA_PATH) \
	E2E_PREFIX=$(E2E_PREFIX) \
	E2E_REGION=$(E2E_REGION) \
	E2E_LOCATION=$(E2E_LOCATION) \
	./hack/e2e-check-clean.sh

coverage:
	go tool cover -html coverage.out -o coverage.html

tidy: ## Run "go mod tidy"
	go mod tidy

download: ## Run "go mod download"
	go mod download

codegen: ## Auto generate files based on GCP APIs
	./hack/codegen.sh

crds: ## Apply CRDs
	kubectl apply -f charts/karpenter/crds/

.PHONY: help presubmit run ut-test require-project-id e2e-setup e2e-tests e2e-test e2e-teardown e2e-check-clean e2e-deploy coverage update verify-codegen verify image apply delete toolchain tidy download

define newline


endef

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
PROJECT_ID ?= karpenter-provider-gcp
CLUSTER_NAME ?= karpenter-provider-gcp
REGION ?= us-central1

# Common Directories
MOD_DIRS = $(shell find . -path "./website" -prune -o -name go.mod -type f -print | xargs dirname)
KARPENTER_CORE_DIR = $(shell go list -m -f '{{ .Dir }}' sigs.k8s.io/karpenter)

# TEST_SUITE enables you to select a specific test suite directory to run "make e2etests" against
TEST_SUITE ?= "..."

help: ## Display help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

presubmit: update verify ut-test ## Run all steps in the developer loop

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

verify: ## Verify code. Includes linting, formatting, etc
	golangci-lint run --timeout=20m

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
		-cover -coverprofile=coverage.out -outputdir=. -coverpkg=./...

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

.PHONY: help presubmit run ut-test coverage update verify image apply delete toolchain tidy download

define newline


endef

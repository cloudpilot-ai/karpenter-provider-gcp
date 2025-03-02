#!/usr/bin/env bash

set -eu -o pipefail

# Update CRD
controller-gen crd paths=./pkg/apis/v1alpha1/... output:crd:dir=./charts/karpenter/crds
controller-gen crd paths=sigs.k8s.io/karpenter/pkg/apis/v1/... output:crd:dir=./charts/karpenter/crds

# Update generated code
export REPO_ROOT=$(pwd)
export GOPATH="${REPO_ROOT}/_go"

cleanup() {
  # TODO: In github action, it needs root privilege to delete this dir
  sudo rm -rf "${GOPATH}"
}
trap "cleanup" EXIT SIGINT

KARPENTER_GO_PACKAGE="github.com/cloudpilot-ai/karpenter-provider-gcp"
GO_PKG_DIR=$(dirname "${GOPATH}/src/${KARPENTER_GO_PACKAGE}")
mkdir -p "${GO_PKG_DIR}"

if [[ ! -e "${GO_PKG_DIR}" || "$(readlink "${GO_PKG_DIR}")" != "${REPO_ROOT}" ]]; then
  ln -snf "${REPO_ROOT}" "${GO_PKG_DIR}"
fi

deepcopy-gen \
  --go-header-file hack/boilerplate.go.txt \
  --output-file-base zz_generated.deepcopy \
  --input-dirs github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1

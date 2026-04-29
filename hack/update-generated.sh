#!/usr/bin/env bash

set -eu -o pipefail

# Update CRD — GCP-specific types
controller-gen crd paths=./pkg/apis/v1alpha1/... output:crd:dir=./charts/karpenter/crds

# Copy karpenter-core CRDs from vendored pre-built files rather than re-generating
# from source.  Re-generating via controller-gen would produce a truncated operator
# enum for NodeSelectorRequirementWithMinValues.Operator ([Gte,Lte] only) because the
# upstream marker (+kubebuilder:validation:Enum:=Gte;Lte) interacts differently with
# every controller-gen version. The vendored files are the canonical artifacts shipped
# by the karpenter-core team and always have the correct full enum.
KARPENTER_CRD_DIR=vendor/sigs.k8s.io/karpenter/pkg/apis/crds
cp "${KARPENTER_CRD_DIR}"/*.yaml ./charts/karpenter/crds/

# Update generated code
export REPO_ROOT=$(pwd)
export GOPATH="${REPO_ROOT}/_go"

cleanup() {
  chmod -R u+w "${GOPATH}" 2>/dev/null || true
  rm -rf "${GOPATH}"
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

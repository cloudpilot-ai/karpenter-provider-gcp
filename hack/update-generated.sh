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

# Sync CRDs to karpenter-crd chart with additionalAnnotations support
rm -f charts/karpenter-crd/templates/*.yaml
cp charts/karpenter/crds/*.yaml charts/karpenter-crd/templates/
hack/mutation/crd_annotations.sh

# Update generated code
deepcopy-gen \
  --go-header-file hack/boilerplate.go.txt \
  --output-file zz_generated.deepcopy.go \
  github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1

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

# GKE provides the CapacityBuffer CRD natively from 1.35.2-gke.1842000 onward.
# charts/karpenter/crds/ is Helm's unconditional crds/ directory — it can't be
# templated, so a version (or feature-gate) condition isn't possible there.
# It's shipped, gated, via templates/ in both charts instead.
rm -f ./charts/karpenter/crds/autoscaling.x-k8s.io_capacitybuffers.yaml
rm -f charts/karpenter/templates/autoscaling.x-k8s.io_capacitybuffers.yaml
hack/mutation/crd_capacitybuffer_maintemplate.sh

# Sync CRDs to karpenter-crd chart with additionalAnnotations support
rm -f charts/karpenter-crd/templates/*.yaml
cp charts/karpenter/crds/*.yaml charts/karpenter-crd/templates/
cp "${KARPENTER_CRD_DIR}"/autoscaling.x-k8s.io_capacitybuffers.yaml charts/karpenter-crd/templates/
hack/mutation/crd_annotations.sh
hack/mutation/crd_capacitybuffer_gate.sh

# Update generated code
deepcopy-gen \
  --go-header-file hack/boilerplate.go.txt \
  --output-file zz_generated.deepcopy.go \
  github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1

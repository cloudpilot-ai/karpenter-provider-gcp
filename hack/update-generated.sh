#!/usr/bin/env bash

set -eu -o pipefail

# Update CRDs
controller-gen crd paths=./pkg/apis/v1alpha1/... output:crd:dir=./charts/karpenter/crds
controller-gen crd paths=sigs.k8s.io/karpenter/pkg/apis/v1/... output:crd:dir=./charts/karpenter/crds

# Update generated deepcopy methods
controller-gen object:headerFile=hack/boilerplate.go.txt paths=./pkg/apis/v1alpha1/...

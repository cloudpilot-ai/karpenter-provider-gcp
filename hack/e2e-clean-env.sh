#!/usr/bin/env bash
# Remove resources left by e2e suites without tearing down the cluster or controller.
set -euo pipefail

: "${E2E_PROJECT_ID:?E2E_PROJECT_ID must be set}"
: "${E2E_LOCATION:?E2E_LOCATION must be set}"
E2E_PREFIX="${E2E_PREFIX:-karpenter-e2e}"
expected_context="gke_${E2E_PROJECT_ID}_${E2E_LOCATION}_${E2E_PREFIX}-cluster"
current_context="$(kubectl config current-context)"
if [[ "${current_context}" != "${expected_context}" ]]; then
  echo "ERROR: refusing cleanup: context ${current_context} is not ${expected_context}" >&2
  exit 1
fi

# NodeClaim finalizers delete their VMs; leave unrelated and legacy unlabeled resources untouched.
for item in 'nodeclaims.karpenter.sh nodeclaims' 'nodepools.karpenter.sh nodepools' 'nodeoverlays.karpenter.sh nodeoverlays' 'gcenodeclasses.karpenter.k8s.gcp gcenodeclasses'; do
  read -r crd resource <<< "${item}"
  installed="$(kubectl get crd "${crd}" -o name --ignore-not-found)"
  if [[ -n "${installed}" ]]; then
    kubectl delete "${resource}" --selector=karpenter-e2e/owned=true --ignore-not-found --wait=true --timeout=5m
  fi
done
kubectl delete namespace karpenter-e2e-test --ignore-not-found --wait=true --timeout=5m

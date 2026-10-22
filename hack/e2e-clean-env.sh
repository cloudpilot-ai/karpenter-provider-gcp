#!/usr/bin/env bash
# Remove resources left by e2e suites without tearing down the cluster or controller.
# Sourcing defines the phase; standalone execution acquires the canonical Lease.

# The coordinator validates ownership/cloud inventory before calling this phase.
# Arguments are the captured, label-owned Deployment names, not arbitrary workloads.
e2e_clean_env() (
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

  # Preserve the test namespace and bootstrap RBAC. Foreground GC drains only
  # captured owned roots and their UID-correlated ReplicaSets/Pods.
  for deployment in "$@"; do
    kubectl -n karpenter-e2e-test delete deployments.apps \
      --field-selector="metadata.name=${deployment}" --selector=karpenter-e2e/owned=true \
      --cascade=foreground --ignore-not-found --wait=true --timeout=5m
  done

  # NodeClaim finalizers and the old controller delete their VMs/disks. Never
  # remove finalizers or delete cloud resources directly.
  for resource in nodepools.karpenter.sh nodeclaims.karpenter.sh nodeoverlays.karpenter.sh gcenodeclasses.karpenter.k8s.gcp gcecustommachinetypes.karpenter.k8s.gcp; do
    installed="$(kubectl get crd "${resource}" -o name --ignore-not-found)"
    if [[ -n "${installed}" ]]; then
      kubectl delete "${resource}" --selector=karpenter-e2e/owned=true --ignore-not-found --wait=true --timeout=5m
    fi
  done
)

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  set -euo pipefail
  cd "$(dirname "${BASH_SOURCE[0]}")/.."
  exec go run ./hack/e2e-runner local-clean-env "$@"
fi

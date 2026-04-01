#!/usr/bin/env bash
# e2e-check-clean.sh — Reports any GCP resources created by e2e-setup.sh that
# still exist. Exits 0 if clean, 1 if orphaned resources are found.
# Does NOT delete anything.
#
# Required:
#   E2E_PROJECT_ID  GCP project ID
#
# Optional (with defaults matching e2e-setup.sh):
#   GOOGLE_APPLICATION_CREDENTIALS  path to service-account key JSON (optional)
#   E2E_PREFIX      resource name prefix  (default: karpenter-e2e)
#   E2E_REGION      GCP region            (default: us-central1)
#   E2E_ZONE        GCP zone              (default: <region>-a)
set -euo pipefail

: "${E2E_PROJECT_ID:?E2E_PROJECT_ID must be set}"

E2E_PREFIX="${E2E_PREFIX:-karpenter-e2e}"
E2E_REGION="${E2E_REGION:-us-central1}"
E2E_ZONE="${E2E_ZONE:-${E2E_REGION}-a}"

CLUSTER_NAME="${E2E_PREFIX}-cluster"
NETWORK_NAME="${E2E_PREFIX}-vpc"
SUBNET_NAME="${E2E_PREFIX}-subnet"
GSA_ID="${E2E_PREFIX}-karpenter"
GSA_EMAIL="${GSA_ID}@${E2E_PROJECT_ID}.iam.gserviceaccount.com"
AR_REPO="${E2E_PREFIX}-images"

if [ -n "${GOOGLE_APPLICATION_CREDENTIALS:-}" ]; then
  gcloud auth activate-service-account \
    --key-file "${GOOGLE_APPLICATION_CREDENTIALS}" \
    --project "${E2E_PROJECT_ID}" \
    --quiet 2>/dev/null
fi

found=0

report() {
  local resource="$1"; shift
  echo "  FOUND: ${resource} — $*" >&2
  found=1
}

echo "Checking for orphaned e2e resources in project ${E2E_PROJECT_ID}..." >&2

# ── GKE Cluster ────────────────────────────────────────────────────────────────
status="$(gcloud container clusters describe "${CLUSTER_NAME}" \
  --zone "${E2E_ZONE}" --project "${E2E_PROJECT_ID}" \
  --format='value(status)' 2>/dev/null || true)"
if [ -n "${status}" ]; then
  report "GKE cluster" "${CLUSTER_NAME} (status=${status})"
fi

# ── Karpenter-provisioned compute instances ────────────────────────────────────
# These are named "karpenter-<nodepool>-<suffix>" and live in the e2e zone.
instances="$(gcloud compute instances list \
  --project "${E2E_PROJECT_ID}" \
  --filter="name~^karpenter- AND zone:${E2E_ZONE}" \
  --format='value(name,zone,status)' 2>/dev/null || true)"
if [ -n "${instances}" ]; then
  while IFS= read -r line; do
    report "Compute instance" "${line}"
  done <<< "${instances}"
fi

# ── Persistent disks not attached to any instance ─────────────────────────────
# GKE node disks are named after the instance; orphaned ones linger after failed
# node deletion and are billed even when unattached.
orphan_disks="$(gcloud compute disks list \
  --project "${E2E_PROJECT_ID}" \
  --filter="zone:${E2E_ZONE} AND name~^karpenter- AND -users:*" \
  --format='value(name,sizeGb,status)' 2>/dev/null || true)"
if [ -n "${orphan_disks}" ]; then
  while IFS= read -r line; do
    report "Orphaned disk" "${line}"
  done <<< "${orphan_disks}"
fi

# ── Artifact Registry repo ─────────────────────────────────────────────────────
if gcloud artifacts repositories describe "${AR_REPO}" \
    --location "${E2E_REGION}" --project "${E2E_PROJECT_ID}" \
    &>/dev/null 2>&1; then
  report "Artifact Registry repo" "${AR_REPO} in ${E2E_REGION}"
fi

# ── Service account ────────────────────────────────────────────────────────────
if gcloud iam service-accounts describe "${GSA_EMAIL}" \
    --project "${E2E_PROJECT_ID}" &>/dev/null 2>&1; then
  report "Service account" "${GSA_EMAIL}"
fi

# ── Subnet ─────────────────────────────────────────────────────────────────────
if gcloud compute networks subnets describe "${SUBNET_NAME}" \
    --region "${E2E_REGION}" --project "${E2E_PROJECT_ID}" \
    &>/dev/null 2>&1; then
  report "Subnet" "${SUBNET_NAME} in ${E2E_REGION}"
fi

# ── VPC ────────────────────────────────────────────────────────────────────────
if gcloud compute networks describe "${NETWORK_NAME}" \
    --project "${E2E_PROJECT_ID}" &>/dev/null 2>&1; then
  report "VPC network" "${NETWORK_NAME}"
fi

# ── Result ─────────────────────────────────────────────────────────────────────
if [ "${found}" -eq 0 ]; then
  echo "OK: No orphaned e2e resources found." >&2
  exit 0
else
  echo "" >&2
  echo "Run hack/e2e-teardown.sh to remove the above resources." >&2
  exit 1
fi

#!/usr/bin/env bash
# e2e-check-clean.sh — Reports any GCP resources created by e2e-setup.sh that
# still exist. Exits 0 if clean, 1 if orphaned resources are found.
# Does NOT delete anything.
#
# Optional (with defaults matching e2e-setup.sh):
#   GOOGLE_APPLICATION_CREDENTIALS  path to service-account key JSON
#   E2E_PROJECT_ID  GCP project ID  (default: parsed from credentials)
#   E2E_PREFIX      resource name prefix  (default: karpenter-e2e)
#   E2E_REGION      GCP region            (default: us-central1)
#   E2E_LOCATION    GCP location (zone or region)
set -euo pipefail

if [ -n "${GOOGLE_APPLICATION_CREDENTIALS:-}" ]; then
  gcloud auth activate-service-account \
    --key-file "${GOOGLE_APPLICATION_CREDENTIALS}" \
    --quiet 2>/dev/null
fi

# E2E_PROJECT_ID can be set explicitly; if not, extract it from the credentials file.
if [ -z "${E2E_PROJECT_ID:-}" ]; then
  if [ -z "${GOOGLE_APPLICATION_CREDENTIALS:-}" ]; then
    echo "ERROR: E2E_PROJECT_ID is not set and GOOGLE_APPLICATION_CREDENTIALS is not set — cannot determine project." >&2
    exit 1
  fi
  E2E_PROJECT_ID="$(python3 -c "import json; print(json.load(open('${GOOGLE_APPLICATION_CREDENTIALS}'))['project_id'])")" \
    || { echo "ERROR: could not parse project_id from ${GOOGLE_APPLICATION_CREDENTIALS}" >&2; exit 1; }
fi

: "${E2E_LOCATION:?E2E_LOCATION must be set (zone, e.g. us-central1-f, or region, e.g. us-central1)}"
E2E_PREFIX="${E2E_PREFIX:-karpenter-e2e}"
E2E_REGION="${E2E_REGION:-us-central1}"

CLUSTER_NAME="${E2E_PREFIX}-cluster"
NETWORK_NAME="${E2E_PREFIX}-vpc"
SUBNET_NAME="${E2E_PREFIX}-subnet"
GSA_ID="${E2E_PREFIX}-karpenter"
GSA_EMAIL="${GSA_ID}@${E2E_PROJECT_ID}.iam.gserviceaccount.com"
AR_REPO="${E2E_PREFIX}-images"

found=0

report() {
  local resource="$1"; shift
  echo "  FOUND: ${resource} — $*" >&2
  found=1
}

echo "Checking for orphaned e2e resources in project ${E2E_PROJECT_ID}..." >&2

# GKE cluster
status="$(gcloud container clusters describe "${CLUSTER_NAME}" \
  --location "${E2E_LOCATION}" --project "${E2E_PROJECT_ID}" \
  --format='value(status)' 2>/dev/null || true)"
if [ -n "${status}" ]; then
  report "GKE cluster" "${CLUSTER_NAME} (status=${status})"
fi

# Compute instances (named "karpenter-<nodepool>-<suffix>")
instances="$(gcloud compute instances list \
  --project "${E2E_PROJECT_ID}" \
  --filter="name~^karpenter- AND zone:${E2E_LOCATION}" \
  --format='value(name,zone,status)' 2>/dev/null || true)"
if [ -n "${instances}" ]; then
  while IFS= read -r line; do
    report "Compute instance" "${line}"
  done <<< "${instances}"
fi

# Orphaned disks (billed even when unattached)
orphan_disks="$(gcloud compute disks list \
  --project "${E2E_PROJECT_ID}" \
  --filter="zone:${E2E_LOCATION} AND name~^karpenter- AND -users:*" \
  --format='value(name,sizeGb,status)' 2>/dev/null || true)"
if [ -n "${orphan_disks}" ]; then
  while IFS= read -r line; do
    report "Orphaned disk" "${line}"
  done <<< "${orphan_disks}"
fi

# Artifact Registry repo
if gcloud artifacts repositories describe "${AR_REPO}" \
    --location "${E2E_REGION}" --project "${E2E_PROJECT_ID}" \
    &>/dev/null; then
  report "Artifact Registry repo" "${AR_REPO} in ${E2E_REGION}"
fi

# Service account
if gcloud iam service-accounts describe "${GSA_EMAIL}" \
    --project "${E2E_PROJECT_ID}" &>/dev/null; then
  report "Service account" "${GSA_EMAIL}"
fi

# Subnet
if gcloud compute networks subnets describe "${SUBNET_NAME}" \
    --region "${E2E_REGION}" --project "${E2E_PROJECT_ID}" \
    &>/dev/null; then
  report "Subnet" "${SUBNET_NAME} in ${E2E_REGION}"
fi

# VPC
if gcloud compute networks describe "${NETWORK_NAME}" \
    --project "${E2E_PROJECT_ID}" &>/dev/null; then
  report "VPC network" "${NETWORK_NAME}"
fi

if [ "${found}" -eq 0 ]; then
  echo "OK: No orphaned e2e resources found." >&2
  exit 0
else
  echo "" >&2
  echo "Run hack/e2e-teardown.sh to remove the above resources." >&2
  exit 1
fi

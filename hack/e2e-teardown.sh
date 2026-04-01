#!/usr/bin/env bash
# e2e-teardown.sh — Deletes all GCP infra created by e2e-setup.sh.
# Order matters: Helm uninstall first, then cluster (so GKE removes its
# firewall rules/routes), then VPC.
#
# Required:
#   GOOGLE_APPLICATION_CREDENTIALS  path to service-account key JSON
#   E2E_PROJECT_ID                  GCP project ID
#
# Optional (with defaults matching e2e-setup.sh):
#   E2E_PREFIX   (default: karpenter-e2e)
#   E2E_REGION   (default: us-central1)
#   E2E_ZONE     (default: <region>-a)
set -euo pipefail

: "${GOOGLE_APPLICATION_CREDENTIALS:?GOOGLE_APPLICATION_CREDENTIALS must be set}"
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

log() { echo "e2e-teardown: $*" >&2; }

gcloud auth activate-service-account \
  --key-file "${GOOGLE_APPLICATION_CREDENTIALS}" \
  --project "${E2E_PROJECT_ID}" \
  --quiet

# ── Helm Uninstall ─────────────────────────────────────────────────────────────
# Uninstall before deleting the cluster so the karpenter termination controller
# has a chance to clean up GCP instances it created.
if gcloud container clusters describe "${CLUSTER_NAME}" \
    --zone "${E2E_ZONE}" --project "${E2E_PROJECT_ID}" &>/dev/null; then
  log "Fetching cluster credentials for Helm uninstall..."
  gcloud container clusters get-credentials "${CLUSTER_NAME}" \
    --zone "${E2E_ZONE}" \
    --project "${E2E_PROJECT_ID}" \
    --quiet

  if helm status karpenter -n karpenter-system &>/dev/null; then
    log "Uninstalling karpenter Helm release..."
    helm uninstall karpenter -n karpenter-system --wait --timeout 5m || true
  fi
fi

# ── GKE Cluster ────────────────────────────────────────────────────────────────
if gcloud container clusters describe "${CLUSTER_NAME}" \
    --zone "${E2E_ZONE}" --project "${E2E_PROJECT_ID}" &>/dev/null; then
  log "Deleting cluster ${CLUSTER_NAME}..."
  gcloud container clusters delete "${CLUSTER_NAME}" \
    --zone "${E2E_ZONE}" \
    --project "${E2E_PROJECT_ID}" \
    --quiet

  log "Waiting for cluster deletion to complete..."
  while gcloud container clusters describe "${CLUSTER_NAME}" \
      --zone "${E2E_ZONE}" --project "${E2E_PROJECT_ID}" &>/dev/null; do
    sleep 15
  done
  log "Cluster deleted."
else
  log "Cluster ${CLUSTER_NAME} not found, skipping."
fi

# ── Artifact Registry ──────────────────────────────────────────────────────────
# Always deleted to avoid paying for stored images between runs.
if gcloud artifacts repositories describe "${AR_REPO}" \
    --location "${E2E_REGION}" --project "${E2E_PROJECT_ID}" &>/dev/null; then
  log "Deleting Artifact Registry repo ${AR_REPO}..."
  gcloud artifacts repositories delete "${AR_REPO}" \
    --location "${E2E_REGION}" \
    --project "${E2E_PROJECT_ID}" \
    --quiet
fi

# ── IAM Bindings ───────────────────────────────────────────────────────────────
log "Removing IAM bindings for ${GSA_EMAIL}..."
for role in roles/compute.admin roles/container.admin roles/iam.serviceAccountUser; do
  gcloud projects remove-iam-policy-binding "${E2E_PROJECT_ID}" \
    --member "serviceAccount:${GSA_EMAIL}" \
    --role "${role}" \
    --condition=None \
    --quiet 2>/dev/null || true
done

# ── Service Account ────────────────────────────────────────────────────────────
if gcloud iam service-accounts describe "${GSA_EMAIL}" \
    --project "${E2E_PROJECT_ID}" &>/dev/null; then
  log "Deleting service account ${GSA_EMAIL}..."
  gcloud iam service-accounts delete "${GSA_EMAIL}" \
    --project "${E2E_PROJECT_ID}" \
    --quiet
fi

# ── Subnet ─────────────────────────────────────────────────────────────────────
if gcloud compute networks subnets describe "${SUBNET_NAME}" \
    --region "${E2E_REGION}" --project "${E2E_PROJECT_ID}" &>/dev/null; then
  log "Deleting subnet ${SUBNET_NAME}..."
  gcloud compute networks subnets delete "${SUBNET_NAME}" \
    --region "${E2E_REGION}" \
    --project "${E2E_PROJECT_ID}" \
    --quiet
fi

# ── VPC ────────────────────────────────────────────────────────────────────────
if gcloud compute networks describe "${NETWORK_NAME}" \
    --project "${E2E_PROJECT_ID}" &>/dev/null; then
  log "Deleting network ${NETWORK_NAME}..."
  gcloud compute networks delete "${NETWORK_NAME}" \
    --project "${E2E_PROJECT_ID}" \
    --quiet
fi

log "Teardown complete."

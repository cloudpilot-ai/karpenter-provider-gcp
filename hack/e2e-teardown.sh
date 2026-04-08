#!/usr/bin/env bash
# e2e-teardown.sh — Deletes all GCP infra created by e2e-setup.sh.
# Order matters: Helm uninstall first, then cluster (so GKE removes its
# firewall rules/routes), then VPC.
#
# Required:
#   GOOGLE_APPLICATION_CREDENTIALS  path to service-account key JSON
#
# Optional (with defaults matching e2e-setup.sh):
#   E2E_PROJECT_ID  GCP project ID  (default: parsed from credentials)
#   E2E_PREFIX      (default: karpenter-e2e)
#   E2E_REGION      (default: us-central1)
#   E2E_LOCATION    GCP location (zone or region)
set -euo pipefail

: "${GOOGLE_APPLICATION_CREDENTIALS:?GOOGLE_APPLICATION_CREDENTIALS must be set}"

log() { echo "e2e-teardown: $*" >&2; }

# E2E_PROJECT_ID can be set explicitly; if not, extract it from the credentials file.
if [ -z "${E2E_PROJECT_ID:-}" ]; then
  E2E_PROJECT_ID="$(python3 -c "import json; print(json.load(open('${GOOGLE_APPLICATION_CREDENTIALS}'))['project_id'])")" \
    || { echo "ERROR: E2E_PROJECT_ID is not set and could not be parsed from ${GOOGLE_APPLICATION_CREDENTIALS}" >&2; exit 1; }
  log "Derived E2E_PROJECT_ID=${E2E_PROJECT_ID} from credentials file"
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

gcloud auth activate-service-account \
  --key-file "${GOOGLE_APPLICATION_CREDENTIALS}" \
  --project "${E2E_PROJECT_ID}" \
  --quiet

# Uninstall karpenter before deleting the cluster so it can clean up GCP instances.
if gcloud container clusters describe "${CLUSTER_NAME}" \
    --location "${E2E_LOCATION}" --project "${E2E_PROJECT_ID}" &>/dev/null; then
  log "Fetching cluster credentials for Helm uninstall..."
  gcloud container clusters get-credentials "${CLUSTER_NAME}" \
    --location "${E2E_LOCATION}" \
    --project "${E2E_PROJECT_ID}" \
    --quiet

  if helm status karpenter -n karpenter-system &>/dev/null; then
    log "Uninstalling karpenter Helm release..."
    if ! helm uninstall karpenter -n karpenter-system --wait --timeout 5m; then
      log "WARNING: helm uninstall failed or timed out — karpenter-managed GCP instances may not have been terminated"
    fi
  fi
fi

# GKE cluster
if gcloud container clusters describe "${CLUSTER_NAME}" \
    --location "${E2E_LOCATION}" --project "${E2E_PROJECT_ID}" &>/dev/null; then
  log "Deleting cluster ${CLUSTER_NAME}..."
  gcloud container clusters delete "${CLUSTER_NAME}" \
    --location "${E2E_LOCATION}" \
    --project "${E2E_PROJECT_ID}" \
    --quiet

  log "Waiting for cluster deletion to complete..."
  WAIT_SECS=0
  MAX_WAIT_SECS=1800  # 30 minutes
  while gcloud container clusters describe "${CLUSTER_NAME}" \
      --location "${E2E_LOCATION}" --project "${E2E_PROJECT_ID}" &>/dev/null; do
    sleep 15
    WAIT_SECS=$((WAIT_SECS + 15))
    if [ "${WAIT_SECS}" -ge "${MAX_WAIT_SECS}" ]; then
      echo "ERROR: cluster ${CLUSTER_NAME} did not finish deleting within ${MAX_WAIT_SECS}s" >&2
      exit 1
    fi
  done
  log "Cluster deleted."
else
  log "Cluster ${CLUSTER_NAME} not found, skipping."
fi

# Artifact Registry
if gcloud artifacts repositories describe "${AR_REPO}" \
    --location "${E2E_REGION}" --project "${E2E_PROJECT_ID}" &>/dev/null; then
  log "Deleting Artifact Registry repo ${AR_REPO}..."
  gcloud artifacts repositories delete "${AR_REPO}" \
    --location "${E2E_REGION}" \
    --project "${E2E_PROJECT_ID}" \
    --quiet
fi

log "Removing IAM bindings for ${GSA_EMAIL}..."
for role in roles/compute.admin roles/container.admin roles/iam.serviceAccountUser; do
  gcloud projects remove-iam-policy-binding "${E2E_PROJECT_ID}" \
    --member "serviceAccount:${GSA_EMAIL}" \
    --role "${role}" \
    --condition=None \
    --quiet || true
done

# Service account
if gcloud iam service-accounts describe "${GSA_EMAIL}" \
    --project "${E2E_PROJECT_ID}" &>/dev/null; then
  log "Deleting service account ${GSA_EMAIL}..."
  gcloud iam service-accounts delete "${GSA_EMAIL}" \
    --project "${E2E_PROJECT_ID}" \
    --quiet
fi

# GKE cluster deletion is async: even after the cluster API object disappears,
# GKE-managed instance groups may still hold the subnet for a minute or two.
# Retry with backoff so we don't fail when re-running teardown after a partial run.
if gcloud compute networks subnets describe "${SUBNET_NAME}" \
    --region "${E2E_REGION}" --project "${E2E_PROJECT_ID}" &>/dev/null; then
  log "Deleting subnet ${SUBNET_NAME}..."
  WAIT_SECS=0
  MAX_WAIT_SECS=600
  until gcloud compute networks subnets delete "${SUBNET_NAME}" \
      --region "${E2E_REGION}" \
      --project "${E2E_PROJECT_ID}" \
      --quiet 2>/dev/null; do
    if [ "${WAIT_SECS}" -ge "${MAX_WAIT_SECS}" ]; then
      echo "ERROR: subnet ${SUBNET_NAME} still in use after ${MAX_WAIT_SECS}s — GKE instance groups may not have fully cleaned up" >&2
      exit 1
    fi
    log "Subnet still in use (GKE instance group cleanup in progress); retrying in 15s (${WAIT_SECS}s elapsed)..."
    sleep 15
    WAIT_SECS=$((WAIT_SECS + 15))
  done
fi

# VPC
if gcloud compute networks describe "${NETWORK_NAME}" \
    --project "${E2E_PROJECT_ID}" &>/dev/null; then
  log "Deleting network ${NETWORK_NAME}..."
  gcloud compute networks delete "${NETWORK_NAME}" \
    --project "${E2E_PROJECT_ID}" \
    --quiet
fi

log "Teardown complete."

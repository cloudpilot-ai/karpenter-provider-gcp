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
#   E2E_ZONE        (default: <region>-a)
set -euo pipefail

: "${GOOGLE_APPLICATION_CREDENTIALS:?GOOGLE_APPLICATION_CREDENTIALS must be set}"

log() { echo "e2e-teardown: $*" >&2; }

# E2E_PROJECT_ID can be set explicitly; if not, extract it from the credentials file.
if [ -z "${E2E_PROJECT_ID:-}" ]; then
  E2E_PROJECT_ID="$(python3 -c "import json; print(json.load(open('${GOOGLE_APPLICATION_CREDENTIALS}'))['project_id'])")" \
    || { echo "ERROR: E2E_PROJECT_ID is not set and could not be parsed from ${GOOGLE_APPLICATION_CREDENTIALS}" >&2; exit 1; }
  log "Derived E2E_PROJECT_ID=${E2E_PROJECT_ID} from credentials file"
fi

E2E_PREFIX="${E2E_PREFIX:-karpenter-e2e}"
E2E_REGION="${E2E_REGION:-us-central1}"
E2E_ZONE="${E2E_ZONE:-${E2E_REGION}-a}"

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
    if ! helm uninstall karpenter -n karpenter-system --wait --timeout 5m; then
      log "WARNING: helm uninstall failed or timed out — karpenter-managed GCP instances may not have been terminated"
    fi
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
  WAIT_SECS=0
  MAX_WAIT_SECS=1800  # 30 minutes
  while gcloud container clusters describe "${CLUSTER_NAME}" \
      --zone "${E2E_ZONE}" --project "${E2E_PROJECT_ID}" &>/dev/null; do
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
    --quiet || true
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

# ── Cloud Logging ──────────────────────────────────────────────────────────────
# Delete all Cloud Logging entries written by the cluster. We first collect the
# distinct log names that have entries with our cluster's resource label, then
# delete each named log. gcloud has no per-entry delete — deleting the log name
# removes all entries in it project-wide, which is fine in a dedicated e2e
# project. In a shared project this would also remove entries from other
# resources that share the same log name; add --force if needed or skip.
log "Deleting Cloud Logging data for cluster ${CLUSTER_NAME}..."
LOG_NAMES=$(gcloud logging entries list \
    --project="${E2E_PROJECT_ID}" \
    --filter="resource.labels.cluster_name=\"${CLUSTER_NAME}\"" \
    --format="value(logName)" \
    --limit=1000 2>/dev/null | sort -u)
if [ -n "${LOG_NAMES}" ]; then
  while IFS= read -r log_name; do
    [ -z "${log_name}" ] && continue
    log "  Deleting log ${log_name}..."
    gcloud logging logs delete "${log_name}" \
        --project="${E2E_PROJECT_ID}" --quiet 2>/dev/null || \
      log "  WARNING: could not delete log ${log_name} (may already be gone)"
  done <<< "${LOG_NAMES}"
  log "Cloud Logging cleanup done."
else
  log "No Cloud Logging entries found for cluster ${CLUSTER_NAME} (already clean or logging was disabled)."
fi

# Cloud Monitoring timeseries data (GKE built-in metrics such as
# kubernetes.io/container/*) cannot be deleted via API — they expire
# automatically after their retention period (typically 6 weeks).
# Custom metric descriptors created by the cluster are deleted below.
log "Deleting custom Cloud Monitoring metric descriptors for cluster ${CLUSTER_NAME}..."
CUSTOM_METRICS=$(gcloud monitoring metrics-descriptors list \
    --project="${E2E_PROJECT_ID}" \
    --filter="metric.type:\"custom.googleapis.com\" AND description~\"${CLUSTER_NAME}\"" \
    --format="value(name)" 2>/dev/null || true)
if [ -n "${CUSTOM_METRICS}" ]; then
  while IFS= read -r metric; do
    [ -z "${metric}" ] && continue
    gcloud monitoring metrics-descriptors delete "${metric}" \
        --project="${E2E_PROJECT_ID}" --quiet 2>/dev/null || true
  done <<< "${CUSTOM_METRICS}"
  log "Custom metric descriptors deleted."
else
  log "No custom metric descriptors found (timeseries data expires automatically)."
fi

log "Teardown complete."

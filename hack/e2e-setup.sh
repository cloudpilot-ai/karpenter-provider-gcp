#!/usr/bin/env bash
# e2e-setup.sh — Idempotently creates GCP infra for e2e tests and deploys
# the karpenter controller via Helm. Reuses existing resources on re-runs.
#
# Required:
#   GOOGLE_APPLICATION_CREDENTIALS  path to service-account key JSON
#
# Optional (with defaults):
#   E2E_PROJECT_ID    GCP project ID        (default: parsed from credentials)
#   E2E_PREFIX        resource name prefix  (default: karpenter-e2e)
#   E2E_REGION        GCP region            (default: us-central1)
#   E2E_ZONE          GCP zone              (default: <region>-a)
#   E2E_MACHINE_TYPE  system node type      (default: n2-standard-2)
set -euo pipefail

: "${GOOGLE_APPLICATION_CREDENTIALS:?GOOGLE_APPLICATION_CREDENTIALS must be set}"

log() { echo "e2e-setup: $*" >&2; }

# E2E_PROJECT_ID can be set explicitly; if not, extract it from the credentials file.
if [ -z "${E2E_PROJECT_ID:-}" ]; then
  E2E_PROJECT_ID="$(python3 -c "import json; print(json.load(open('${GOOGLE_APPLICATION_CREDENTIALS}'))['project_id'])")" \
    || { echo "ERROR: E2E_PROJECT_ID is not set and could not be parsed from ${GOOGLE_APPLICATION_CREDENTIALS}" >&2; exit 1; }
  log "Derived E2E_PROJECT_ID=${E2E_PROJECT_ID} from credentials file"
fi

E2E_PREFIX="${E2E_PREFIX:-karpenter-e2e}"
E2E_REGION="${E2E_REGION:-us-central1}"
E2E_ZONE="${E2E_ZONE:-${E2E_REGION}-a}"
E2E_MACHINE_TYPE="${E2E_MACHINE_TYPE:-n2-standard-2}"

# Derived names — must match Makefile variables exactly.
CLUSTER_NAME="${E2E_PREFIX}-cluster"
NETWORK_NAME="${E2E_PREFIX}-vpc"
SUBNET_NAME="${E2E_PREFIX}-subnet"
PODS_RANGE="${E2E_PREFIX}-pods"
SERVICES_RANGE="${E2E_PREFIX}-services"
GSA_ID="${E2E_PREFIX}-karpenter"
GSA_EMAIL="${GSA_ID}@${E2E_PROJECT_ID}.iam.gserviceaccount.com"
AR_REPO="${E2E_PREFIX}-images"
IMAGE_REPO="${E2E_REGION}-docker.pkg.dev/${E2E_PROJECT_ID}/${AR_REPO}/karpenter"

PRIMARY_CIDR="10.0.0.0/20"
PODS_CIDR="10.4.0.0/14"
SERVICES_CIDR="10.8.0.0/20"

REPO_ROOT="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"

log "Authenticating..."
gcloud auth activate-service-account \
  --key-file "${GOOGLE_APPLICATION_CREDENTIALS}" \
  --project "${E2E_PROJECT_ID}" \
  --quiet

# Verify the project exists and is accessible; avoids silent hangs on a wrong ID.
if ! gcloud projects describe "${E2E_PROJECT_ID}" &>/dev/null; then
  echo "ERROR: GCP project '${E2E_PROJECT_ID}' not found or not accessible. Verify E2E_PROJECT_ID." >&2
  exit 1
fi
log "Verified project ${E2E_PROJECT_ID}"

# VPC
if gcloud compute networks describe "${NETWORK_NAME}" \
    --project "${E2E_PROJECT_ID}" --format='value(name)' &>/dev/null; then
  log "Reusing network ${NETWORK_NAME}"
else
  log "Creating network ${NETWORK_NAME}..."
  gcloud compute networks create "${NETWORK_NAME}" \
    --project "${E2E_PROJECT_ID}" \
    --subnet-mode=custom \
    --quiet
fi

# Subnet
if gcloud compute networks subnets describe "${SUBNET_NAME}" \
    --region "${E2E_REGION}" --project "${E2E_PROJECT_ID}" \
    --format='value(name)' &>/dev/null; then
  log "Reusing subnet ${SUBNET_NAME}"
else
  log "Creating subnet ${SUBNET_NAME}..."
  gcloud compute networks subnets create "${SUBNET_NAME}" \
    --network "${NETWORK_NAME}" \
    --region "${E2E_REGION}" \
    --range "${PRIMARY_CIDR}" \
    --secondary-range "${PODS_RANGE}=${PODS_CIDR},${SERVICES_RANGE}=${SERVICES_CIDR}" \
    --project "${E2E_PROJECT_ID}" \
    --quiet
fi

# Service account
if gcloud iam service-accounts describe "${GSA_EMAIL}" \
    --project "${E2E_PROJECT_ID}" &>/dev/null; then
  log "Reusing service account ${GSA_EMAIL}"
else
  log "Creating service account ${GSA_ID}..."
  gcloud iam service-accounts create "${GSA_ID}" \
    --project "${E2E_PROJECT_ID}" \
    --display-name "Karpenter e2e controller" \
    --quiet
fi

log "Ensuring IAM bindings for ${GSA_EMAIL}..."
for role in roles/compute.admin roles/container.admin roles/iam.serviceAccountUser; do
  gcloud projects add-iam-policy-binding "${E2E_PROJECT_ID}" \
    --member "serviceAccount:${GSA_EMAIL}" \
    --role "${role}" \
    --condition=None \
    --quiet >/dev/null
done

# Artifact Registry
if gcloud artifacts repositories describe "${AR_REPO}" \
    --location "${E2E_REGION}" --project "${E2E_PROJECT_ID}" &>/dev/null; then
  log "Reusing Artifact Registry repo ${AR_REPO}"
else
  log "Creating Artifact Registry repo ${AR_REPO}..."
  gcloud artifacts repositories create "${AR_REPO}" \
    --repository-format=docker \
    --location "${E2E_REGION}" \
    --project "${E2E_PROJECT_ID}" \
    --quiet
fi

# Allow the karpenter GSA to pull from this registry.
gcloud artifacts repositories add-iam-policy-binding "${AR_REPO}" \
  --location "${E2E_REGION}" \
  --project "${E2E_PROJECT_ID}" \
  --member "serviceAccount:${GSA_EMAIL}" \
  --role roles/artifactregistry.reader \
  --quiet >/dev/null

# GKE cluster
CLUSTER_STATUS="$(gcloud container clusters describe "${CLUSTER_NAME}" \
  --zone "${E2E_ZONE}" --project "${E2E_PROJECT_ID}" \
  --format='value(status)' 2>/dev/null || echo NOT_FOUND)"

case "${CLUSTER_STATUS}" in
  RUNNING)
    log "Reusing cluster ${CLUSTER_NAME}"
    ;;
  STOPPING)
    # A STOPPING cluster transitions to NOT_FOUND (deleted), never to RUNNING.
    # Waiting for RUNNING would hang forever — fail fast so the caller can
    # decide whether to re-run setup after the deletion completes.
    echo "ERROR: cluster ${CLUSTER_NAME} is being deleted (STOPPING). Run e2e-teardown.sh then retry." >&2
    exit 1
    ;;
  RECONCILING|PROVISIONING)
    log "Cluster ${CLUSTER_NAME} is in state ${CLUSTER_STATUS}, waiting for RUNNING..."
    WAIT_SECS=0
    MAX_WAIT_SECS=1200  # 20 minutes
    while true; do
      sleep 15
      WAIT_SECS=$((WAIT_SECS + 15))
      if [ "${WAIT_SECS}" -ge "${MAX_WAIT_SECS}" ]; then
        echo "ERROR: cluster ${CLUSTER_NAME} did not reach RUNNING within ${MAX_WAIT_SECS}s (last status: ${CLUSTER_STATUS})" >&2
        exit 1
      fi
      CLUSTER_STATUS="$(gcloud container clusters describe "${CLUSTER_NAME}" \
        --zone "${E2E_ZONE}" --project "${E2E_PROJECT_ID}" \
        --format='value(status)' 2>/dev/null || echo NOT_FOUND)"
      log "  cluster status: ${CLUSTER_STATUS} (${WAIT_SECS}s elapsed)"
      [ "${CLUSTER_STATUS}" = "RUNNING" ] && break
    done
    log "Cluster ${CLUSTER_NAME} is now RUNNING"
    ;;
  NOT_FOUND)
    log "Creating cluster ${CLUSTER_NAME} (this may take ~10 min)..."
    gcloud container clusters create "${CLUSTER_NAME}" \
      --zone "${E2E_ZONE}" \
      --project "${E2E_PROJECT_ID}" \
      --network "${NETWORK_NAME}" \
      --subnetwork "${SUBNET_NAME}" \
      --cluster-secondary-range-name "${PODS_RANGE}" \
      --services-secondary-range-name "${SERVICES_RANGE}" \
      --enable-ip-alias \
      --workload-pool "${E2E_PROJECT_ID}.svc.id.goog" \
      --release-channel regular \
      --machine-type "${E2E_MACHINE_TYPE}" \
      --disk-size 30 \
      --num-nodes 1 \
      --no-enable-cloud-logging \
      --no-enable-cloud-monitoring \
      --quiet
    ;;
  *)
    echo "ERROR: cluster ${CLUSTER_NAME} is in unexpected state: ${CLUSTER_STATUS}" >&2
    exit 1
    ;;
esac

log "Fetching cluster credentials..."
gcloud container clusters get-credentials "${CLUSTER_NAME}" \
  --zone "${E2E_ZONE}" \
  --project "${E2E_PROJECT_ID}" \
  --quiet

log "Ensuring test namespace..."
kubectl create namespace karpenter-e2e-test --dry-run=client -o yaml | kubectl apply -f -

# Drop any manually-created karpenter SA so Helm can own it.
kubectl delete serviceaccount karpenter -n karpenter-system --ignore-not-found

log "Ensuring workload identity binding..."
gcloud iam service-accounts add-iam-policy-binding "${GSA_EMAIL}" \
  --role roles/iam.workloadIdentityUser \
  --member "serviceAccount:${E2E_PROJECT_ID}.svc.id.goog[karpenter-system/karpenter]" \
  --project "${E2E_PROJECT_ID}" \
  --quiet >/dev/null

GOOGLE_APPLICATION_CREDENTIALS="${GOOGLE_APPLICATION_CREDENTIALS}" \
E2E_PROJECT_ID="${E2E_PROJECT_ID}" \
E2E_PREFIX="${E2E_PREFIX}" \
E2E_REGION="${E2E_REGION}" \
E2E_ZONE="${E2E_ZONE}" \
  "${REPO_ROOT}/hack/e2e-deploy.sh"

log ""
log "Setup complete."
log "Run tests with:"
log "  make e2etests"

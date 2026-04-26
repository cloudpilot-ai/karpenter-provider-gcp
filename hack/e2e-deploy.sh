#!/usr/bin/env bash
# e2e-deploy.sh — Build the karpenter image and deploy it via Helm onto the
# existing e2e cluster. Safe to run repeatedly (idempotent helm upgrade).
#
# Required env vars (set by e2e-setup.sh or the caller):
#   GOOGLE_APPLICATION_CREDENTIALS  path to service-account key JSON
#   E2E_PROJECT_ID                  GCP project ID
#   E2E_PREFIX                      resource name prefix (default: karpenter-e2e)
#   E2E_REGION                      GCP region          (default: us-central1)
#   E2E_LOCATION                    GCP location (zone or region)
set -euo pipefail

: "${GOOGLE_APPLICATION_CREDENTIALS:?GOOGLE_APPLICATION_CREDENTIALS must be set}"
: "${E2E_PROJECT_ID:?E2E_PROJECT_ID must be set}"

: "${E2E_LOCATION:?E2E_LOCATION must be set (zone, e.g. us-central1-f, or region, e.g. us-central1)}"
E2E_PREFIX="${E2E_PREFIX:-karpenter-e2e}"
E2E_REGION="${E2E_REGION:-us-central1}"

CLUSTER_NAME="${E2E_PREFIX}-cluster"
GSA_ID="${E2E_PREFIX}-karpenter"
GSA_EMAIL="${GSA_ID}@${E2E_PROJECT_ID}.iam.gserviceaccount.com"
AR_REPO="${E2E_PREFIX}-images"
IMAGE_REPO="${E2E_REGION}-docker.pkg.dev/${E2E_PROJECT_ID}/${AR_REPO}/karpenter"

REPO_ROOT="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"

log() { echo "e2e-deploy: $*" >&2; }

log "Authenticating Docker to Artifact Registry..."
gcloud auth configure-docker "${E2E_REGION}-docker.pkg.dev" --quiet

log "Building and pushing karpenter image with ko..."
KO_IMAGE_TAG="e2e-$(git rev-parse --short HEAD 2>/dev/null || echo latest)"
IMAGE_REF="$(
  KO_DOCKER_REPO="${IMAGE_REPO}" \
  GOFLAGS="" \
    ko build --bare \
    --platform linux/amd64 \
    --tags "${KO_IMAGE_TAG}" \
    github.com/cloudpilot-ai/karpenter-provider-gcp/cmd/controller
)"
log "Image: ${IMAGE_REF}"

log "Deploying karpenter via Helm..."
helm upgrade --install karpenter "${REPO_ROOT}/charts/karpenter" \
  --namespace karpenter-system \
  --create-namespace \
  --set controller.image.repository="${IMAGE_REPO}" \
  --set controller.image.tag="${KO_IMAGE_TAG}" \
  --set logLevel=debug \
  --set controller.settings.projectID="${E2E_PROJECT_ID}" \
  --set controller.settings.clusterName="${CLUSTER_NAME}" \
  --set controller.settings.clusterLocation="${E2E_LOCATION}" \
  --set controller.featureGates.spotToSpotConsolidation=true \
  --set controller.featureGates.nodeRepair=true \
  --set "serviceAccount.annotations.iam\\.gke\\.io/gcp-service-account=${GSA_EMAIL}" \
  --set controller.replicaCount=1 \
  --set credentials.enabled=false \
  --wait \
  --timeout 5m

log "Deploy complete. IMAGE_REF=${IMAGE_REF}"

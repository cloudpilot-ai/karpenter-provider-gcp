#!/usr/bin/env bash
# e2e-deploy.sh — Deploy karpenter onto the existing e2e cluster.
#
# Two modes:
#   RELEASE_VERSION unset  — build image from local source (ko) and deploy
#                            from local chart files. Used for feature branches.
#   RELEASE_VERSION=X.Y.Z  — install the published Helm chart at that version.
#                            No local build. Used to validate published releases.
#
# Required env vars:
#   GOOGLE_APPLICATION_CREDENTIALS  path to service-account key JSON
#   E2E_PROJECT_ID                  GCP project ID
#   E2E_LOCATION                    GCP location (zone or region)
#
# Optional env vars:
#   RELEASE_VERSION   published chart version to install (e.g. 0.3.2)
#   E2E_PREFIX        resource name prefix (default: karpenter-e2e)
#   E2E_REGION        GCP region (default: us-central1)
set -euo pipefail

: "${GOOGLE_APPLICATION_CREDENTIALS:?GOOGLE_APPLICATION_CREDENTIALS must be set}"
: "${E2E_PROJECT_ID:?E2E_PROJECT_ID must be set}"
: "${E2E_LOCATION:?E2E_LOCATION must be set}"

E2E_PREFIX="${E2E_PREFIX:-karpenter-e2e}"
E2E_REGION="${E2E_REGION:-us-central1}"
RELEASE_VERSION="${RELEASE_VERSION:-}"

CLUSTER_NAME="${E2E_PREFIX}-cluster"
GSA_ID="${E2E_PREFIX}-karpenter"
GSA_EMAIL="${GSA_ID}@${E2E_PROJECT_ID}.iam.gserviceaccount.com"

REPO_ROOT="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"

log() { echo "e2e-deploy: $*" >&2; }

# Common helm values regardless of mode
HELM_COMMON_ARGS=(
  --namespace karpenter-system
  --create-namespace
  --set logLevel=debug
  --set controller.settings.projectID="${E2E_PROJECT_ID}"
  --set controller.settings.clusterName="${CLUSTER_NAME}"
  --set controller.settings.clusterLocation="${E2E_LOCATION}"
  --set controller.featureGates.spotToSpotConsolidation=true
  --set controller.featureGates.nodeRepair=true
  --set "serviceAccount.annotations.iam\\.gke\\.io/gcp-service-account=${GSA_EMAIL}"
  --set controller.replicaCount=1
  --set credentials.enabled=false
  --wait
  --timeout 5m
)

if [[ -n "${RELEASE_VERSION}" ]]; then
  # --- Published release mode ---
  log "Installing published release ${RELEASE_VERSION} from Helm repo..."
  helm repo add karpenter-gcp "https://cloudpilot-ai.github.io/karpenter-provider-gcp" --force-update
  helm repo update karpenter-gcp

  helm upgrade --install karpenter-crd karpenter-gcp/karpenter-crd \
    --version "${RELEASE_VERSION}" \
    --namespace karpenter-system \
    --create-namespace \
    --wait \
    --timeout 2m

  helm upgrade --install karpenter karpenter-gcp/karpenter \
    --version "${RELEASE_VERSION}" \
    "${HELM_COMMON_ARGS[@]}"

  log "Deploy complete. karpenter ${RELEASE_VERSION} installed from published chart."
else
  # --- Local build mode ---
  AR_REPO="${E2E_PREFIX}-images"
  IMAGE_REPO="${E2E_REGION}-docker.pkg.dev/${E2E_PROJECT_ID}/${AR_REPO}/karpenter"

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

  helm upgrade --install karpenter-crd "${REPO_ROOT}/charts/karpenter-crd" \
    --namespace karpenter-system \
    --create-namespace \
    --wait \
    --timeout 2m

  helm upgrade --install karpenter "${REPO_ROOT}/charts/karpenter" \
    --set controller.image.repository="${IMAGE_REPO}" \
    --set controller.image.tag="${KO_IMAGE_TAG}" \
    --set controller.image.digest="${IMAGE_REF##*@}" \
    "${HELM_COMMON_ARGS[@]}"

  log "Deploy complete. IMAGE_REF=${IMAGE_REF}"
fi

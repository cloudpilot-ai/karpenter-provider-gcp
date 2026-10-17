# Installation

This guide walks you through deploying Karpenter GCP on a GKE cluster using Helm.

## Prerequisites

- A running GKE cluster (Standard mode, Kubernetes 1.28+)
- `gcloud` CLI configured with project access
- `kubectl` pointing at your cluster
- `helm` v3.x

Enable the required APIs:

```sh
gcloud services enable compute.googleapis.com container.googleapis.com
```

## Step 1 — Create a GCP service account

Karpenter needs a minimal set of GCP permissions to manage Compute Engine instances and read GKE cluster configuration. The canonical permission list is in [`deploy/iam/karpenter-controller-role.yaml`](https://github.com/cloudpilot-ai/karpenter-provider-gcp/blob/main/deploy/iam/karpenter-controller-role.yaml) in the repository — that file is the source of truth for all IAM references.

```sh
export PROJECT_ID=<your-project-id>
export GSA_NAME=karpenter-gsa

gcloud iam service-accounts create $GSA_NAME --project=$PROJECT_ID

# Create the minimal custom role from the canonical permission list.
# If upgrading and the role already exists, use `gcloud iam roles update` with the same flags.
curl -fsSL https://raw.githubusercontent.com/cloudpilot-ai/karpenter-provider-gcp/main/deploy/iam/karpenter-controller-role.yaml \
    -o karpenter-controller-role.yaml
gcloud iam roles create karpenter_controller \
    --project=$PROJECT_ID \
    --file=karpenter-controller-role.yaml

gcloud projects add-iam-policy-binding $PROJECT_ID \
    --member="serviceAccount:$GSA_NAME@$PROJECT_ID.iam.gserviceaccount.com" \
    --role="projects/$PROJECT_ID/roles/karpenter_controller"

# Create a dedicated node SA with the GKE-recommended minimal permissions.
# This replaces the Compute Engine default SA (which has broad editor-equivalent access).
export NODE_SA_NAME=karpenter-node

gcloud iam service-accounts create $NODE_SA_NAME --project=$PROJECT_ID
export NODE_SA_EMAIL=$NODE_SA_NAME@$PROJECT_ID.iam.gserviceaccount.com

# roles/container.nodeServiceAccount bundles the minimum GKE node permissions:
# logging.logWriter, monitoring.metricWriter, monitoring.viewer,
# stackdriver.resourceMetadata.writer.
gcloud projects add-iam-policy-binding $PROJECT_ID \
    --member="serviceAccount:$NODE_SA_EMAIL" \
    --role="roles/container.nodeServiceAccount"

# If nodes pull images from Artifact Registry, also grant read access:
# gcloud projects add-iam-policy-binding $PROJECT_ID \
#     --member="serviceAccount:$NODE_SA_EMAIL" \
#     --role="roles/artifactregistry.reader"

# iam.serviceAccountUser must be scoped to each SA Karpenter may attach to nodes.
# If you also use GCENodeClass.spec.serviceAccount or --default-nodepool-service-account
# to set a different node SA, grant iam.serviceAccountUser on that SA as well.
gcloud iam service-accounts add-iam-policy-binding $NODE_SA_EMAIL \
    --role roles/iam.serviceAccountUser \
    --member "serviceAccount:$GSA_NAME@$PROJECT_ID.iam.gserviceaccount.com" \
    --project $PROJECT_ID
```

Tell Karpenter to use the dedicated node SA by adding this flag to the `helm upgrade` command in Step 2:

```sh
--set "controller.settings.defaultNodepoolServiceAccount=$NODE_SA_EMAIL" \
```

Or set it per-NodeClass via `GCENodeClass.spec.serviceAccount: <email>`.

> **Fallback:** If you skip node SA creation, Karpenter uses the Compute Engine default SA (`<project-number>-compute@developer.gserviceaccount.com`), which has broad `roles/editor`-equivalent permissions. This is not recommended for production clusters.

## Step 2 — Install Karpenter with Helm

```sh
export PROJECT_ID=<your-project-id>
export CLUSTER_NAME=<your-cluster-name>
export REGION=<your-region-or-zone>   # e.g. us-central1 or us-central1-f
```

To verify signatures and build provenance first, follow [Verify release artifacts](#verify-release-artifacts-optional) instead of the quick installation commands below.

```sh
helm repo add karpenter-provider-gcp https://cloudpilot-ai.github.io/karpenter-provider-gcp
helm repo update

# Install CRDs first (separate chart ensures CRDs are upgraded on helm upgrade)
helm upgrade --install karpenter-crd karpenter-provider-gcp/karpenter-crd

# Install the controller
helm upgrade karpenter karpenter-provider-gcp/karpenter --install \
  --namespace karpenter-system --create-namespace \
  --set "controller.settings.projectID=${PROJECT_ID}" \
  --set "controller.settings.clusterLocation=${REGION}" \
  --set "controller.settings.clusterName=${CLUSTER_NAME}" \
  --set "credentials.enabled=false" \
  --set "serviceAccount.annotations.iam\.gke\.io/gcp-service-account=$GSA_NAME@${PROJECT_ID}.iam.gserviceaccount.com" \
  --wait
```

## Step 3 — Bind Workload Identity

Allow the Karpenter Kubernetes service account to impersonate the GCP service account:

```sh
gcloud iam service-accounts add-iam-policy-binding \
    $GSA_NAME@$PROJECT_ID.iam.gserviceaccount.com \
    --role roles/iam.workloadIdentityUser \
    --member "serviceAccount:$PROJECT_ID.svc.id.goog[karpenter-system/karpenter]"
```

## Step 4 — Verify

```sh
kubectl get pods -n karpenter-system
```

Both the controller and webhook pods should reach `Running` state. Check logs if they don't:

```sh
kubectl logs -n karpenter-system deployment/karpenter
```

## Alternative: service account keys

If Workload Identity is not available in your environment, you can use a JSON key file instead.

Create the service account and download a key:

```sh
gcloud iam service-accounts keys create key.json \
    --iam-account=$GSA_NAME@$PROJECT_ID.iam.gserviceaccount.com
```

Create a Kubernetes Secret from the key:

```sh
kubectl create namespace karpenter-system
kubectl create secret generic karpenter-gcp-credentials \
    --namespace karpenter-system \
    --from-file=key.json=key.json
```

Install without `credentials.enabled=false` (the default enables key-based auth):

```sh
helm upgrade karpenter karpenter-provider-gcp/karpenter --install \
  --namespace karpenter-system --create-namespace \
  --set "controller.settings.projectID=${PROJECT_ID}" \
  --set "controller.settings.clusterLocation=${REGION}" \
  --set "controller.settings.clusterName=${CLUSTER_NAME}" \
  --wait
```

> **Warning:** Service account keys are long-lived credentials. Prefer Workload Identity wherever possible.

## Verify release artifacts (optional)

You can verify release artifacts before installation. Controller images use keyless Cosign signatures; Helm charts use OpenPGP provenance. Both have GitHub build attestations. Releases published before signing was introduced are unsigned.

These examples require [Cosign v3.1.3](https://github.com/sigstore/cosign/releases/tag/v3.1.3), [crane](https://github.com/google/go-containerregistry/tree/main/cmd/crane), GnuPG, Helm, and an authenticated [GitHub CLI](https://cli.github.com/). The Cosign flags select the legacy signature format used by this repository. Stop if any signature or attestation check fails.

Replace `vX.Y.Z` with a signed release tag, including its leading `v`:

```sh
export VERSION="vX.Y.Z"
export CHART_VERSION="${VERSION#v}"
export REPOSITORY="cloudpilot-ai/karpenter-provider-gcp"
export RELEASE_WORKFLOW="${REPOSITORY}/.github/workflows/release-build.yaml"
```

### Verify the controller image

Resolve the tag once and verify that immutable image digest against the release workflow and tag:

```sh
export IMAGE_REPOSITORY="public.ecr.aws/cloudpilotai/gcp/karpenter"
IMAGE_DIGEST=$(crane digest "${IMAGE_REPOSITORY}:${VERSION}")
IMAGE="${IMAGE_REPOSITORY}@${IMAGE_DIGEST}"
cosign verify "${IMAGE}" \
  --new-bundle-format=false --experimental-oci11=false \
  --certificate-identity "https://github.com/${RELEASE_WORKFLOW}@refs/tags/${VERSION}" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

### Verify the Helm charts

The project's signing-key fingerprint is `7D575660AC2C6650E263439577A23E34D5FE007A`. Trust this reviewed fingerprint, not a fingerprint supplied by an unverified chart repository. Import the downloaded public material into a temporary GnuPG home, then export **only** that trusted key for Helm:

```sh
export CHARTS_URL="https://cloudpilot-ai.github.io/karpenter-provider-gcp"
export FINGERPRINT="7D575660AC2C6650E263439577A23E34D5FE007A"
curl --fail --location "${CHARTS_URL}/keys/${FINGERPRINT}.asc" --output karpenter-signing-key.asc
KEY_HOME=$(mktemp -d)
gpg --homedir "${KEY_HOME}" --batch --import karpenter-signing-key.asc
gpg --homedir "${KEY_HOME}" --batch --export "${FINGERPRINT}" > karpenter-signing-keyring.gpg
rm -rf "${KEY_HOME}"

helm repo add karpenter-provider-gcp "${CHARTS_URL}"
helm repo update karpenter-provider-gcp
helm pull karpenter-provider-gcp/karpenter-crd --version "${CHART_VERSION}" --verify --keyring karpenter-signing-keyring.gpg
helm pull karpenter-provider-gcp/karpenter --version "${CHART_VERSION}" --verify --keyring karpenter-signing-keyring.gpg
```

Stop if either verification fails. For a release signed by another key, obtain its trusted fingerprint from the maintainers before changing the keyring; do not automatically trust keys listed in `signed-charts.txt`.

### Verify build provenance

Check that the same image digest and downloaded chart archives were built from the selected tag by the release workflow. GitHub CLI authentication is required:

```sh
gh attestation verify "oci://${IMAGE}" --repo "${REPOSITORY}" \
  --signer-workflow "${RELEASE_WORKFLOW}" --source-ref "refs/tags/${VERSION}"
gh attestation verify "karpenter-crd-${CHART_VERSION}.tgz" --repo "${REPOSITORY}" \
  --signer-workflow "${RELEASE_WORKFLOW}" --source-ref "refs/tags/${VERSION}"
gh attestation verify "karpenter-${CHART_VERSION}.tgz" --repo "${REPOSITORY}" \
  --signer-workflow "${RELEASE_WORKFLOW}" --source-ref "refs/tags/${VERSION}"
```

Build provenance identifies the source and workflow; it does not establish reproducibility or absence of vulnerabilities.

### Install the verified artifacts

Use the verified local archives rather than downloading the charts again, and pin the controller to the verified image digest. With the cluster variables and Workload Identity settings from Steps 1–2:

```sh
helm upgrade --install karpenter-crd "./karpenter-crd-${CHART_VERSION}.tgz" \
  --verify --keyring karpenter-signing-keyring.gpg
helm upgrade --install karpenter "./karpenter-${CHART_VERSION}.tgz" \
  --namespace karpenter-system --create-namespace \
  --verify --keyring karpenter-signing-keyring.gpg \
  --set "controller.settings.projectID=${PROJECT_ID}" \
  --set "controller.settings.clusterLocation=${REGION}" \
  --set "controller.settings.clusterName=${CLUSTER_NAME}" \
  --set "credentials.enabled=false" \
  --set "serviceAccount.annotations.iam\.gke\.io/gcp-service-account=$GSA_NAME@${PROJECT_ID}.iam.gserviceaccount.com" \
  --set-string "controller.image.repository=${IMAGE_REPOSITORY}" \
  --set-string "controller.image.tag=${VERSION}@${IMAGE_DIGEST}" \
  --wait
```

Keep any additional values from Step 2, including the dedicated node service account setting when configured. The image reference includes both the release tag and digest (`repository:tag@sha256:...`); the digest determines which image is pulled. Continue with [Step 3 — Bind Workload Identity](#step-3--bind-workload-identity).

## Upgrading

See the [upgrade guide](upgrading.md).

## Uninstalling

```sh
helm uninstall karpenter --namespace karpenter-system
kubectl delete namespace karpenter-system
```

## Next steps

- [Quick start](quick-start.md) — create a NodePool and trigger your first provisioning event

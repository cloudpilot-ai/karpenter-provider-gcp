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

# iam.serviceAccountUser must be scoped to each SA that Karpenter may attach to nodes.
# By default, Karpenter uses the Compute Engine default SA (broad editor-equivalent permissions).
# We recommend creating a dedicated minimal-privilege node SA instead — see the note below.
#
# If you override the node SA via GCENodeClass.spec.serviceAccount or the
# --node-pool-service-account controller flag, grant iam.serviceAccountUser on that SA too.
#
# To find the Compute Engine default SA:
#   echo "$(gcloud projects describe $PROJECT_ID --format='value(projectNumber)')-compute@developer.gserviceaccount.com"
export NODE_SA_EMAIL=<your-node-sa>@<your-project-id>.iam.gserviceaccount.com
gcloud iam service-accounts add-iam-policy-binding $NODE_SA_EMAIL \
    --role roles/iam.serviceAccountUser \
    --member "serviceAccount:$GSA_NAME@$PROJECT_ID.iam.gserviceaccount.com" \
    --project $PROJECT_ID
```

> **Recommendation:** The Compute Engine default SA has broad `roles/editor`-equivalent permissions. Create a dedicated node SA with only the permissions your workloads need and pass it via `GCENodeClass.spec.serviceAccount` or the `--node-pool-service-account` controller flag.

## Step 2 — Install Karpenter with Helm

```sh
export PROJECT_ID=<your-project-id>
export CLUSTER_NAME=<your-cluster-name>
export REGION=<your-region-or-zone>   # e.g. us-central1 or us-central1-f

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

## Upgrading

See the [upgrade guide](upgrading.md).

## Uninstalling

```sh
helm uninstall karpenter --namespace karpenter-system
kubectl delete namespace karpenter-system
```

## Next steps

- [Quick start](quick-start.md) — create a NodePool and trigger your first provisioning event

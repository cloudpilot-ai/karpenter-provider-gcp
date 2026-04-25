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

Karpenter needs permissions to manage Compute Engine instances and GKE node pools.

```sh
export PROJECT_ID=<your-project-id>
export GSA_NAME=karpenter-gsa

gcloud iam service-accounts create $GSA_NAME --project=$PROJECT_ID

gcloud projects add-iam-policy-binding $PROJECT_ID \
    --member="serviceAccount:$GSA_NAME@$PROJECT_ID.iam.gserviceaccount.com" \
    --role="roles/compute.admin"

gcloud projects add-iam-policy-binding $PROJECT_ID \
    --member="serviceAccount:$GSA_NAME@$PROJECT_ID.iam.gserviceaccount.com" \
    --role="roles/container.admin"

gcloud projects add-iam-policy-binding $PROJECT_ID \
    --member="serviceAccount:$GSA_NAME@$PROJECT_ID.iam.gserviceaccount.com" \
    --role="roles/iam.serviceAccountUser"
```

## Step 2 — Install the CRD chart

Karpenter's CRDs ship as a separate `karpenter-crd` chart so they can be upgraded via `helm upgrade` (Helm treats files in a chart's `crds/` directory as install-only and never updates them on subsequent upgrades). Install it first, at the same version as the main `karpenter` chart:

```sh
export KARPENTER_VERSION=<chart-version>   # same version used for the main chart below

helm repo add karpenter-provider-gcp https://cloudpilot-ai.github.io/karpenter-provider-gcp
helm repo update

helm install karpenter-crd karpenter-provider-gcp/karpenter-crd \
  --version "${KARPENTER_VERSION}" \
  --namespace karpenter-system --create-namespace
```

## Step 3 — Install Karpenter with Helm

```sh
export PROJECT_ID=<your-project-id>
export CLUSTER_NAME=<your-cluster-name>
export REGION=<your-region-or-zone>   # e.g. us-central1 or us-central1-f

helm upgrade karpenter karpenter-provider-gcp/karpenter --install \
  --version "${KARPENTER_VERSION}" \
  --namespace karpenter-system --create-namespace \
  --set "controller.settings.projectID=${PROJECT_ID}" \
  --set "controller.settings.clusterLocation=${REGION}" \
  --set "controller.settings.clusterName=${CLUSTER_NAME}" \
  --set "credentials.enabled=false" \
  --set "serviceAccount.annotations.iam\.gke\.io/gcp-service-account=$GSA_NAME@${PROJECT_ID}.iam.gserviceaccount.com" \
  --wait
```

## Step 4 — Bind Workload Identity

Allow the Karpenter Kubernetes service account to impersonate the GCP service account:

```sh
gcloud iam service-accounts add-iam-policy-binding \
    $GSA_NAME@$PROJECT_ID.iam.gserviceaccount.com \
    --role roles/iam.workloadIdentityUser \
    --member "serviceAccount:$PROJECT_ID.svc.id.goog[karpenter-system/karpenter]"
```

## Step 5 — Verify

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

Install the CRD chart first (per Step 2 above), then install the main chart without `credentials.enabled=false` (the default enables key-based auth):

```sh
helm upgrade karpenter karpenter-provider-gcp/karpenter --install \
  --version "${KARPENTER_VERSION}" \
  --namespace karpenter-system --create-namespace \
  --set "controller.settings.projectID=${PROJECT_ID}" \
  --set "controller.settings.clusterLocation=${REGION}" \
  --set "controller.settings.clusterName=${CLUSTER_NAME}" \
  --wait
```

> **Warning:** Service account keys are long-lived credentials. Prefer Workload Identity wherever possible.

## Uninstalling

```sh
helm uninstall karpenter --namespace karpenter-system
helm uninstall karpenter-crd --namespace karpenter-system
kubectl delete namespace karpenter-system
```

> **Warning:** Uninstalling `karpenter-crd` deletes the four CRDs and cascade-deletes every `GCENodeClass`, `NodePool`, and `NodeClaim` in the cluster. Only do this if you intend to fully remove Karpenter.

## Upgrading from earlier chart versions?

See [Upgrading](./upgrading.md) for migration guidance from chart versions that bundled CRDs in the main `karpenter` chart.

## Next steps

- [Quick start](quick-start.md) — create a NodePool and trigger your first provisioning event

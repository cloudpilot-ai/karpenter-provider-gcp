# karpenter

Karpenter GCP Cloud Provider — cost-efficient autoscaling for GKE clusters

Karpenter GCP is an open-source node provisioning provider for GKE clusters. It watches for unschedulable pods and automatically provisions cost-efficient GCP nodes to meet workload requirements, removing them when they are no longer needed.

## Prerequisites

- Kubernetes 1.26+
- GKE cluster
- GCP service account with Compute Engine permissions (or Workload Identity)

## Installation

Add the Helm repository:

```bash
helm repo add karpenter-gcp https://cloudpilot-ai.github.io/karpenter-provider-gcp
helm repo update
```

Install the chart:

```bash
helm install karpenter karpenter-gcp/karpenter \
  --namespace karpenter \
  --create-namespace \
  --set controller.settings.projectID=<your-gcp-project> \
  --set controller.settings.clusterName=<your-cluster-name> \
  --set controller.settings.clusterLocation=<your-region-or-zone>
```

## Authentication

### Workload Identity (recommended)

Bind your GCP service account to the Karpenter Kubernetes service account:

```bash
gcloud iam service-accounts add-iam-policy-binding \
  karpenter@<PROJECT_ID>.iam.gserviceaccount.com \
  --role roles/iam.workloadIdentityUser \
  --member "serviceAccount:<PROJECT_ID>.svc.id.goog[karpenter/karpenter]"
```

Then annotate the service account and disable the credentials secret:

```bash
helm install karpenter karpenter-gcp/karpenter \
  --set serviceAccount.annotations."iam\.gke\.io/gcp-service-account"=karpenter@<PROJECT_ID>.iam.gserviceaccount.com \
  --set credentials.enabled=false \
  ...
```

### Service Account Key

Create a Kubernetes secret from your service account key file and reference it:

```bash
kubectl create secret generic karpenter-gcp-credentials \
  --from-file=key.json=/path/to/key.json \
  -n karpenter
```

```bash
helm install karpenter karpenter-gcp/karpenter \
  --set credentials.secretName=karpenter-gcp-credentials \
  ...
```

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| additionalAnnotations | object | `{}` | Additional annotations to add into metadata. |
| controller.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[0].labelSelector.matchLabels."app.kubernetes.io/name" | string | `"karpenter"` |  |
| controller.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[0].topologyKey | string | `"kubernetes.io/hostname"` |  |
| controller.env | list | `[]` |  |
| controller.featureGates.nodeOverlay | bool | `false` | nodeOverlay is ALPHA and is disabled by default. Setting this will allow the use of node overlay to impact scheduling decisions |
| controller.featureGates.nodeRepair | bool | `false` | nodeRepair is ALPHA and is disabled by default. When enabled, Karpenter replaces nodes that fail GKE Node Problem Detector health conditions. |
| controller.featureGates.spotToSpotConsolidation | bool | `true` |  |
| controller.featureGates.staticCapacity | bool | `false` | staticCapacity is ALPHA and is disabled by default. When enabled, a NodePool with spec.replicas set maintains a fixed number of nodes regardless of pod demand (static node pool). consolidationPolicy and consolidateAfter are ignored on static NodePools. |
| controller.healthProbe.port | int | `8081` |  |
| controller.image.pullPolicy | string | `"IfNotPresent"` |  |
| controller.image.repository | string | `"public.ecr.aws/cloudpilotai/gcp/karpenter"` |  |
| controller.image.tag | string | `""` |  |
| controller.metrics.port | int | `8080` |  |
| controller.priorityClassName | string | `"gmp-critical"` |  |
| controller.replicaCount | int | `2` |  |
| controller.resources.requests.cpu | string | `"500m"` |  |
| controller.resources.requests.memory | string | `"500Mi"` |  |
| controller.revisionHistoryLimit | int | `10` |  |
| controller.settings.batchIdleDuration | string | `"1s"` | The maximum amount of time with no new ending pods that if exceeded ends the current batching window. If pods arrive faster than this time, the batching window will be extended up to the maxDuration. If they arrive slower, the pods will be batched separately. |
| controller.settings.batchMaxDuration | string | `"10s"` | The maximum length of a batch window. The longer this is, the more pods we can consider for provisioning at one time which usually results in fewer but larger nodes. |
| controller.settings.clusterLocation | string | `""` | The GCP region for instance type discovery (e.g., us-central1) |
| controller.settings.clusterName | string | `""` | The GCP cluster name. |
| controller.settings.nodeLocation | string | `""` | The exact GCP cluster location for GKE API calls (e.g., us-central1-a for zonal, us-central1 for regional). If not set, defaults to 'clusterLocation' for backward compatibility. |
| controller.settings.projectID | string | `""` | The GCP project ID. |
| controller.settings.vmMemoryOverheadPercent | float | `0.065` | The VM memory overhead as a percent that will be subtracted from the total memory for all instance types. The value of `0.075` equals to 7.5%. |
| controller.strategy.rollingUpdate.maxUnavailable | int | `1` |  |
| controller.terminationGracePeriodSeconds | int | `30` |  |
| controller.tolerations | list | `[]` |  |
| credentials | object | `{"enabled":true,"secretKey":"key.json","secretName":""}` | GCP credentials configuration |
| credentials.enabled | bool | `true` | Enable or disable the use of GCP credentials secret Set to true if you want to use a Kubernetes secret for GCP authentication Set to false to rely on other authentication methods (e.g., Workload Identity, instance metadata) |
| credentials.secretKey | string | `"key.json"` | Key within the secret that contains the service account JSON |
| credentials.secretName | string | `""` | Name of the existing secret containing GCP credentials If not specified, defaults to "<release-name>-gcp-credentials" This secret should contain a service account key file |
| fullnameOverride | string | `""` |  |
| imagePullSecrets | list | `[]` |  |
| logErrorOutputPaths | list | `["stderr"]` | Log errorOutputPaths - defaults to stderr only |
| logLevel | string | `"info"` | Global log level, defaults to 'info' |
| logOutputPaths | list | `["stdout"]` | Log outputPaths - defaults to stdout only |
| nameOverride | string | `""` |  |
| podAnnotations | object | `{}` |  |
| podDisruptionBudget.minAvailable | int | `1` |  |
| podLabels | object | `{}` |  |
| serviceAccount.annotations | object | `{}` |  |
| serviceAccount.automount | bool | `true` |  |
| serviceAccount.create | bool | `true` |  |
| serviceAccount.name | string | `""` |  |

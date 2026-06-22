# Settings Reference

Karpenter for GCP is configured via environment variables passed to the controller pod. Most of these can be set through the Helm chart's `values.yaml`. The table below covers every configurable option.

## Feature Gates

Feature gates control opt-in or experimental behaviors. They are passed as a comma-separated list via the `FEATURE_GATES` environment variable.

| Feature Gate              | Helm value                                        | Default | Status | Description                                                                                                                                                                                    |
|---------------------------|---------------------------------------------------|---------|--------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `NodeRepair`              | `controller.featureGates.nodeRepair`              | `false` | Alpha  | Automatically replaces nodes that fail GKE Node Problem Detector health checks.                                                                                                                |
| `ReservedCapacity`        | `controller.featureGates.reservedCapacity`        | `false` | Beta   | Enables scheduling to reserved/committed GCP capacity. Disabled until GCE reservation support is implemented (see [#239](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/239)). |
| `SpotToSpotConsolidation` | `controller.featureGates.spotToSpotConsolidation` | `true`  | Beta   | Allows consolidation to replace a Spot node with a cheaper Spot node (both single- and multi-node).                                                                                            |
| `NodeOverlay`             | `controller.featureGates.nodeOverlay`             | `false` | Alpha  | Applies `NodeOverlay` resources to instance type scheduling decisions.                                                                                                                         |
| `StaticCapacity`          | `controller.featureGates.staticCapacity`          | `false` | Alpha  | Enables NodePools with `spec.replicas` set to maintain a fixed number of nodes regardless of pod demand.                                                                                       |

Example Helm override to enable `NodeRepair`:

```yaml
controller:
  featureGates:
    nodeRepair: true
```

## Controller Options

These options configure the controller's runtime behavior.

| Option                        | Env var                     | Helm value                              | Default | Description                                                                                                                                       |
|-------------------------------|-----------------------------|-----------------------------------------|---------|---------------------------------------------------------------------------------------------------------------------------------------------------|
| `--disable-controller-warmup` | `DISABLE_CONTROLLER_WARMUP` | `controller.disableControllerWarmup`    | `true`  | When `false`, controllers pre-populate caches before winning leader election, reducing failover time. Set to `false` in production for better HA. |
| `--log-level`                 | `LOG_LEVEL`                 | `logLevel`                              | `info`  | Controller log level (`debug`, `info`, `warn`, `error`).                                                                                          |
| `--batch-max-duration`        | `BATCH_MAX_DURATION`        | `controller.settings.batchMaxDuration`  | `10s`   | Maximum time Karpenter batches incoming pods before provisioning.                                                                                 |
| `--batch-idle-duration`       | `BATCH_IDLE_DURATION`       | `controller.settings.batchIdleDuration` | `1s`    | Idle time after the last pod event before Karpenter triggers provisioning.                                                                        |
| `--ignore-dra-requests`       | `IGNORE_DRA_REQUESTS`       | `controller.settings.ignoreDRARequests` | `true`  | When `true`, Karpenter ignores pods' Dynamic Resource Allocation requests during scheduling simulations.                                          |

### Dynamic Resource Allocation (DRA)

Dynamic Resource Allocation (DRA) is a Kubernetes API for requesting and sharing hardware devices — typically GPUs and other accelerators — through `ResourceClaim` objects rather than plain resource requests. Pods reference a claim, and a DRA driver allocates matching devices on nodes that can satisfy it.

Karpenter ignores DRA requests during scheduling simulations by default (`controller.settings.ignoreDRARequests: true`). This preserves scheduling behavior for clusters without DRA drivers, the common case on GKE.

Set `ignoreDRARequests` to `false` only when the cluster has DRA drivers installed and runs workloads that schedule against `ResourceClaim`s. When disabled, Karpenter registers the upstream device-allocation controller and accounts for DRA device availability when provisioning.

```yaml
controller:
  settings:
    ignoreDRARequests: false
```

DRA is a Kubernetes-level feature. Review the [Kubernetes DRA documentation](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/) and your DRA driver's requirements before enabling it.

## GCP-specific Settings

| Setting             | Env var                            | Helm value                                        | Required | Description                                                                                                                                                                                                                                                                                |
|---------------------|------------------------------------|---------------------------------------------------|----------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Project ID          | `PROJECT_ID`                       | `controller.settings.projectID`                   | Yes      | GCP project where nodes are provisioned.                                                                                                                                                                                                                                                   |
| Cluster name        | `CLUSTER_NAME`                     | `controller.settings.clusterName`                 | Yes      | GKE cluster name. Used to scope instance discovery.                                                                                                                                                                                                                                        |
| Cluster location    | `CLUSTER_LOCATION`                 | `controller.settings.clusterLocation`             | Yes      | GKE cluster location (zone or region, e.g. `us-central1-f`).                                                                                                                                                                                                                               |
| Node location       | `NODE_LOCATION`                    | `controller.settings.nodeLocation`                | No       | Override zone for node placement. Defaults to cluster location.                                                                                                                                                                                                                            |
| VM memory overhead  | `VM_MEMORY_OVERHEAD_PERCENT`       | `controller.settings.vmMemoryOverheadPercent`     | No       | Fraction of node memory reserved for OS/kubelet overhead (e.g. `0.075` = 7.5%).                                                                                                                                                                                                            |
| Bootstrap pool name | `DEFAULT_NODEPOOL_TEMPLATE_NAME`   | `controller.settings.defaultNodePoolTemplateName` | No       | Pin the GKE node pool used as the bootstrap metadata source. When set, Karpenter uses this pool exclusively. Leave empty to use automatic discovery (`default-pool` → first alphabetical RUNNING pool → `karpenter-fallback` creation). See [Bootstrap pool selection](bootstrap-pool.md). |
| Default SA          | `DEFAULT_NODEPOOL_SERVICE_ACCOUNT` | `controller.env`                                  | No       | Default service account for provisioned nodes (see [Service account resolution](#service-account-resolution)).                                                                                                                                                                             |

### Service account resolution

Karpenter resolves the service account for provisioned nodes in this order:

1. `spec.serviceAccount` in GCENodeClass (highest priority)
2. `DEFAULT_NODEPOOL_SERVICE_ACCOUNT` environment variable
3. Project's Compute Engine default service account (`<project-number>-compute@developer.gserviceaccount.com`)

If none are available, node provisioning fails.

For production clusters, use a dedicated service account with minimal permissions ([`roles/container.nodeServiceAccount`](https://cloud.google.com/kubernetes-engine/docs/how-to/hardening-your-cluster#use_least_privilege_sa)). Set `DEFAULT_NODEPOOL_SERVICE_ACCOUNT` or `spec.serviceAccount` accordingly.

Example Helm override:

```yaml
controller:
  env:
    - name: DEFAULT_NODEPOOL_SERVICE_ACCOUNT
      value: "my-node-sa@my-project.iam.gserviceaccount.com"
```

## Security Contexts

By default, the Helm chart applies least-privilege security contexts to the controller pod and the `karpenter` container. These follow the Kubernetes Pod Security Standards and let the controller run under the `restricted` profile. Each context is rendered only when its value is non-empty, so you can override individual fields or disable a context entirely by setting it to `{}`.

| Helm value           | Scope     | Default                                                                                                                                                    | Description                                            |
|----------------------|-----------|------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------|
| `podSecurityContext` | Pod       | `runAsNonRoot: true`, `seccompProfile.type: RuntimeDefault`                                                                                                | Security context applied at the pod level.             |
| `securityContext`    | Container | `allowPrivilegeEscalation: false`, `readOnlyRootFilesystem: true`, `runAsNonRoot: true`, `capabilities.drop: [ALL]`, `seccompProfile.type: RuntimeDefault` | Security context applied to the `karpenter` container. |

These are the chart defaults:

```yaml
podSecurityContext:
  runAsNonRoot: true
  seccompProfile:
    type: RuntimeDefault

securityContext:
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  runAsNonRoot: true
  capabilities:
    drop:
      - ALL
  seccompProfile:
    type: RuntimeDefault
```

To override a field, set it under the corresponding key. For example, to pin the user the controller runs as:

```yaml
podSecurityContext:
  runAsNonRoot: true
  runAsUser: 65532
  seccompProfile:
    type: RuntimeDefault
```

To disable a context, set it to an empty object so no `securityContext` block is rendered:

```yaml
podSecurityContext: {}
securityContext: {}
```

## PodDisruptionBudget

The chart creates a PodDisruptionBudget for the controller pods so voluntary disruptions — node drains during upgrades, consolidation, or maintenance — cannot evict every Karpenter replica at once. The default is `minAvailable: 1`, which keeps at least one controller pod running while the others are evicted.

| Helm value                          | Default | Description                                                                                   |
|-------------------------------------|---------|-----------------------------------------------------------------------------------------------|
| `podDisruptionBudget.minAvailable`  | `1`     | Minimum number of controller pods that must stay available. Used when `maxUnavailable` is not set. |
| `podDisruptionBudget.maxUnavailable`| `null`  | Maximum number of controller pods that may be unavailable. When set, takes precedence over `minAvailable`. |

Set `podDisruptionBudget.maxUnavailable` to cap the number of controller pods that may be unavailable instead. When set, `maxUnavailable` takes precedence over `minAvailable`, and the rendered PodDisruptionBudget uses `maxUnavailable` alone. Leave it unset (the default) to keep the `minAvailable` behavior. Both values accept an integer or a percentage string (e.g. `"50%"`).

```yaml
podDisruptionBudget:
  maxUnavailable: 1
```

## NodePool Features

These fields are set on `NodePool` objects, not on the controller. They are part of the karpenter-core API and are available in this provider.

### Karpenter core v1.13

Karpenter core v1.13 adds Dynamic Resource Allocation device allocation tracking and treats Kubernetes Node Readiness Controller taints (`readiness.k8s.io/*`) as ephemeral during scheduling. DRA scheduling remains disabled by default through `controller.settings.ignoreDRARequests: true` — see [Dynamic Resource Allocation (DRA)](#dynamic-resource-allocation-dra).

### Node count limit (v1.11)

`spec.limits.nodes` caps the maximum number of nodes a NodePool will provision, independent of resource limits.

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
spec:
  limits:
    cpu: "100"
    memory: 400Gi
    nodes: "20"       # cap at 20 nodes regardless of resource headroom
```

### Termination grace period (v1.12)

`spec.template.spec.terminationGracePeriod` sets the maximum time Karpenter waits for pods to drain before force-deleting them during disruption. Overrides the pod's own `terminationGracePeriodSeconds` when set.

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
spec:
  template:
    spec:
      terminationGracePeriod: 48h    # wait up to 48 hours for pods to drain
      nodeClassRef:
        group: karpenter.k8s.gcp
        kind: GCENodeClass
        name: default
      requirements:
        - key: kubernetes.io/arch
          operator: In
          values: ["amd64"]
```

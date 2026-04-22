# Settings Reference

Karpenter for GCP is configured via environment variables passed to the controller pod. Most of these can be set through the Helm chart's `values.yaml`. The table below covers every configurable option.

## Feature Gates

Feature gates control opt-in or experimental behaviors. They are passed as a comma-separated list via the `FEATURE_GATES` environment variable.

| Feature Gate              | Helm value                                        | Default | Status | Description                                                                                                                    |
|---------------------------|---------------------------------------------------|---------|--------|--------------------------------------------------------------------------------------------------------------------------------|
| `NodeRepair`              | `controller.featureGates.nodeRepair`              | `false` | Alpha  | Automatically replaces nodes that fail GKE Node Problem Detector health checks.                                                |
| `ReservedCapacity`        | `controller.featureGates.reservedCapacity`        | `true`  | Beta   | Enables scheduling to reserved/committed GCP capacity. Disable only if you do not use Committed Use Discounts or Reservations. |
| `SpotToSpotConsolidation` | `controller.featureGates.spotToSpotConsolidation` | `true`  | Beta   | Allows consolidation to replace a Spot node with a cheaper Spot node (both single- and multi-node).                            |
| `NodeOverlay`             | `controller.featureGates.nodeOverlay`             | `false` | Alpha  | Applies `NodeOverlay` resources to instance type scheduling decisions.                                                         |
| `StaticCapacity`          | `controller.featureGates.staticCapacity`          | `false` | Alpha  | Enables NodePools with `spec.replicas` set to maintain a fixed number of nodes regardless of pod demand.                       |

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

## GCP-specific Settings

| Setting            | Env var                      | Helm value                                    | Required | Description                                                                     |
|--------------------|------------------------------|-----------------------------------------------|----------|---------------------------------------------------------------------------------|
| Project ID         | `PROJECT_ID`                 | `controller.settings.projectID`               | Yes      | GCP project where nodes are provisioned.                                        |
| Cluster name       | `CLUSTER_NAME`               | `controller.settings.clusterName`             | Yes      | GKE cluster name. Used to scope instance discovery.                             |
| Cluster location   | `CLUSTER_LOCATION`           | `controller.settings.clusterLocation`         | Yes      | GKE cluster location (zone or region, e.g. `us-central1-f`).                    |
| Node location      | `NODE_LOCATION`              | `controller.settings.nodeLocation`            | No       | Override zone for node placement. Defaults to cluster location.                 |
| VM memory overhead | `VM_MEMORY_OVERHEAD_PERCENT` | `controller.settings.vmMemoryOverheadPercent` | No       | Fraction of node memory reserved for OS/kubelet overhead (e.g. `0.075` = 7.5%). |

# Advanced examples

## Instance metadata

Set GCE instance metadata key-value pairs for advanced configuration. Values in `spec.metadata` override matching keys from the base instance template; new keys are added.

See [`examples/nodeclass/gcenodeclass-metadata.yaml`](https://github.com/cloudpilot-ai/karpenter-provider-gcp/blob/main/examples/nodeclass/gcenodeclass-metadata.yaml).

For example, if the base GKE node-pool template sets `serial-port-logging-enable=true`, specifying `serial-port-logging-enable: "false"` in `spec.metadata` results in the provisioned instance having `serial-port-logging-enable=false`.

> **Note:** For GPU driver version control, use `spec.gpuDriverVersion` instead of setting `cloud.google.com/gke-gpu-driver-version` via metadata. See [GPU Nodes](../gpu-nodes.md) for details.

## Secondary boot disk (container image pre-loading)

Pre-load container images onto a secondary boot disk to reduce node startup latency. See [GKE container image pre-loading](https://cloud.google.com/kubernetes-engine/docs/how-to/data-container-image-preloading) for how to build the pre-loaded image.

See [`examples/nodeclass/secondary-disk-gcenodeclass.yaml`](https://github.com/cloudpilot-ai/karpenter-provider-gcp/blob/main/examples/nodeclass/secondary-disk-gcenodeclass.yaml).

## Custom kubelet settings

Configure kubelet behavior on provisioned nodes using `spec.kubeletConfiguration`. These settings apply at boot time and inform Karpenter's scheduler for bin-packing calculations.

### Resource reservations

Reserve resources for system daemons and Kubernetes components:

```yaml
kubeletConfiguration:
  systemReserved:
    cpu: "1"
    memory: 1Gi
  kubeReserved:
    cpu: 500m
    memory: 200Mi
```

- `systemReserved` — resources reserved for OS system daemons and kernel memory
- `kubeReserved` — resources reserved for Kubernetes components (kubelet, container runtime)

Both settings reduce node allocatable capacity. The scheduler accounts for these when bin-packing workloads.

When you set a partial `kubeReserved` (for example, only `cpu`), Karpenter preserves provider-computed defaults for unspecified keys like `ephemeral-storage` based on boot disk size.

### Eviction thresholds

Configure kubelet eviction behavior:

```yaml
kubeletConfiguration:
  evictionHard:
    memory.available: 200Mi
    nodefs.available: 10%
  evictionSoft:
    memory.available: 500Mi
    nodefs.available: 15%
  evictionSoftGracePeriod:
    memory.available: 30s
    nodefs.available: 1m
  evictionMaxPodGracePeriod: 60
```

For scheduler bin-packing, only `evictionHard` for `memory.available` and `nodefs.available` reduce calculated allocatable capacity. This matches the AWS Karpenter provider and the [Kubernetes node-pressure-eviction documentation](https://kubernetes.io/docs/concepts/scheduling-eviction/node-pressure-eviction/).

`evictionSoft` thresholds are enforced by the kubelet at runtime but do not affect allocatable calculations.

### Pod density

Control the maximum number of pods per node:

```yaml
kubeletConfiguration:
  maxPods: 50
  podsPerCore: 8
```

- `maxPods` — absolute cap on pods per node
- `podsPerCore` — caps pods at `podsPerCore × cpu_cores`; cannot exceed `maxPods`

Both values are reflected in `node.status.capacity.pods` and used by the scheduler.

### Other settings

Additional kubelet configuration options:

```yaml
kubeletConfiguration:
  clusterDNS:
    - 10.0.1.100
  cpuCFSQuota: false
  imageGCHighThresholdPercent: 85
  imageGCLowThresholdPercent: 80
```

- `clusterDNS` — override cluster DNS server addresses
- `cpuCFSQuota` — enable or disable CPU CFS quota enforcement for containers with CPU limits
- `imageGCHighThresholdPercent` / `imageGCLowThresholdPercent` — control when kubelet garbage-collects unused container images

## Shielded VM

Shielded VM provides verifiable integrity for your instances, protecting against boot-level and kernel-level malware. GCP organizations that enforce `constraints/compute.requireShieldedVm` require these settings on all instances. Without them, Karpenter-provisioned nodes fail with a `412 conditionNotMet` error.

```yaml
shieldedInstanceConfig:
  enableSecureBoot: true
  enableVtpm: true
  enableIntegrityMonitoring: true
```

The options are:

- **enableSecureBoot**: Verifies all boot components (firmware, bootloader, kernel) are signed by trusted publishers. Prevents boot-level rootkits.
- **enableVtpm**: Enables a virtual Trusted Platform Module that validates guest VM integrity before and during boot.
- **enableIntegrityMonitoring**: Monitors and reports changes to the boot sequence. View integrity reports in Cloud Monitoring.

See [GCP Shielded VM documentation](https://cloud.google.com/compute/shielded-vm/docs/shielded-vm) for details on each feature.

## Confidential VM

Confidential VM provides in-use memory encryption via AMD SEV / SEV-SNP or Intel TDX. It is only supported on specific machine families; see the [GCP support matrix](https://cloud.google.com/confidential-computing/confidential-vm/docs/os-and-machine-type) for the current list.

```yaml
confidentialInstanceType: SEV_SNP  # SEV | SEV_SNP | TDX
```

Setting `confidentialInstanceType` enables Confidential VM with the named technology. Karpenter forces `scheduling.onHostMaintenance` to `TERMINATE` on provisioned instances because Confidential VMs cannot live-migrate. Leave the field unset to disable Confidential VM.

If the chosen machine type does not support the requested confidential type, GCE rejects the instance creation and the error surfaces on the `NodeClaim` `Launched` condition.

The default `ContainerOptimizedOS` and `Ubuntu` images boot as Confidential VMs on supported families without any image change. GPU Confidential VMs are an exception: an A3 instance with an attached H100 GPU using Intel TDX requires a TDX-specific image (for example `cos-tdx-*`), which is not available through the `family` image selectors. Pin such an image by its full resource URL with `imageSelectorTerms[].id`; see the [GCP supported configurations](https://cloud.google.com/confidential-computing/confidential-vm/docs/supported-configurations#supported-images-gpu).

## Disk type scheduling

Karpenter applies `disk-type.gke.io/*` labels to provisioned nodes based on the instance type's machine family. These labels indicate which persistent disk types the instance supports, enabling two capabilities:

1. **NodePool requirements** — constrain scheduling to instance types that support specific disk types
2. **PD CSI topology scheduling** — enable StorageClasses with `allowedTopologies` based on disk-type labels

### Constraining instance types by disk support

Use disk-type labels in NodePool requirements to ensure Karpenter selects only instance types that support the disk types your workloads need:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: hyperdisk-workloads
spec:
  template:
    spec:
      nodeClassRef:
        group: karpenter.k8s.gcp
        kind: GCENodeClass
        name: default-example
      requirements:
        - key: disk-type.gke.io/hyperdisk-balanced
          operator: In
          values: ["true"]
```

This NodePool provisions only instance types from families that support Hyperdisk Balanced (such as n2, n4, c3, c4). Instance types from families without Hyperdisk Balanced support (such as e2, n1) are excluded.

### Available disk-type labels

The supported labels correspond to GCP disk types:

| Label                                                   | Disk type                            |
|---------------------------------------------------------|--------------------------------------|
| `disk-type.gke.io/pd-standard`                          | Standard persistent disk             |
| `disk-type.gke.io/pd-balanced`                          | Balanced persistent disk             |
| `disk-type.gke.io/pd-ssd`                               | SSD persistent disk                  |
| `disk-type.gke.io/pd-extreme`                           | Extreme persistent disk              |
| `disk-type.gke.io/hyperdisk-balanced`                   | Hyperdisk Balanced                   |
| `disk-type.gke.io/hyperdisk-extreme`                    | Hyperdisk Extreme                    |
| `disk-type.gke.io/hyperdisk-throughput`                 | Hyperdisk Throughput                 |
| `disk-type.gke.io/hyperdisk-ml`                         | Hyperdisk ML                         |
| `disk-type.gke.io/hyperdisk-balanced-high-availability` | Hyperdisk Balanced High Availability |

Disk compatibility is determined at the machine family level (`n2`, `c3`, `e2`), not the specific instance size. See the [GCP machine family disk support matrix](https://cloud.google.com/compute/docs/disks#disk-types) for which families support which disk types.

### PD CSI topology scheduling

The disk-type labels are also published as topology keys by the GCE Persistent Disk CSI driver when the StorageClass enables disk topology:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: hyperdisk-balanced
provisioner: pd.csi.storage.gke.io
volumeBindingMode: WaitForFirstConsumer
parameters:
  type: hyperdisk-balanced
  use-allowed-disk-topology: "true"
```

StorageClasses with `use-allowed-disk-topology: "true"` can use disk-type labels in `allowedTopologies`, and bound PVs can carry these labels in node affinity so replacement pods schedule only on nodes that support the disk type.

## Multiple NodePools

Run independent pools for different workload tiers, each referencing a different GCENodeClass:

```yaml
# Spot pool for batch workloads
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: batch
spec:
  template:
    metadata:
      labels:
        tier: batch
    spec:
      nodeClassRef:
        group: karpenter.k8s.gcp
        kind: GCENodeClass
        name: default-example
      requirements:
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["spot"]
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    consolidateAfter: 0s
---
# On-demand pool for system workloads
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: system
spec:
  weight: 100   # higher weight = preferred
  template:
    metadata:
      labels:
        tier: system
    spec:
      nodeClassRef:
        group: karpenter.k8s.gcp
        kind: GCENodeClass
        name: default-example
      requirements:
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["on-demand"]
        - key: karpenter.k8s.gcp/instance-family
          operator: In
          values: ["n2"]
  disruption:
    consolidationPolicy: WhenEmpty
    consolidateAfter: 30m
```

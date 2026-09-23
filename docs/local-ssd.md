# Local SSDs

Karpenter can attach [GCE local SSDs](https://cloud.google.com/compute/docs/disks/local-ssd) to the nodes it provisions. Local SSDs are physically attached to the host, so they deliver much higher IOPS and throughput than persistent disks. They are also ephemeral: their data is lost whenever the instance stops, is preempted, or is deleted. Use them for scratch space, caches, and other data you can afford to lose, not for anything that must survive the node.

This page covers how to ask for local SSDs, how their count is chosen, and how they are exposed to workloads.

## How it works

Two independent choices control local SSDs on a node:

- **How they are exposed** — set by `GCENodeClass.spec.localSsdMode`. This decides whether workloads see raw NVMe devices or kubelet ephemeral storage. It does not set the disk count.
- **How many are attached** — decided by the machine type, and for some families by the `karpenter.k8s.gcp/instance-local-ssd-count` scheduling label.

Each local SSD is 375 GiB (the `z3` family uses 3000 GiB partitions). A node's total local SSD capacity is the per-disk size multiplied by the attached count.

> **Note:** GKE Standard is the only supported cluster mode, and these fields apply to the GCE nodes Karpenter provisions. Local SSDs are attached only when the resolved count is greater than zero; on machine types without local SSDs the settings are a no-op.

## Exposure mode

`spec.localSsdMode` selects how GKE exposes attached local SSDs to the node:

| Value       | Exposure                                        | Notes                                                               |
|-------------|-------------------------------------------------|---------------------------------------------------------------------|
| `RawBlock`  | Raw, unformatted NVMe block devices             | Default. Your workload formats and mounts the devices itself.       |
| `Ephemeral` | Kubelet and container-runtime ephemeral storage | Local SSD capacity is advertised as the node's `ephemeral-storage`. |

```yaml
apiVersion: karpenter.k8s.gcp/v1alpha1
kind: GCENodeClass
metadata:
  name: local-ssd
spec:
  localSsdMode: Ephemeral  # or: RawBlock (default)
  imageSelectorTerms:
    - family: ContainerOptimizedOS
      channel: cluster
```

The field defaults to `RawBlock`, so existing NodeClasses are unaffected. Local SSD capacity is reported as `ephemeral-storage` only in `Ephemeral` mode; in `RawBlock` mode a node's `ephemeral-storage` continues to reflect the boot disk size.

## Choosing the disk count

How the count is chosen depends on the machine type.

### Configurable machine types

The `n1`, `n2`, `n2d`, `c2`, and `c2d` families let you choose how many local SSDs to attach. For these families Karpenter publishes a separate scheduling variant of each machine type for every supported count, distinguished by the `karpenter.k8s.gcp/instance-local-ssd-count` label. Count `0` attaches no local SSDs; a positive count attaches that many.

Two things must line up for a positive count to be selected:

1. **The NodePool must opt in.** A NodePool that does not mention `karpenter.k8s.gcp/instance-local-ssd-count` exposes only the count-`0` variant of these families — so it never provisions local SSDs on them. Add the requirement to make the non-zero counts selectable:

   ```yaml
   # NodePool
   spec:
     template:
       spec:
         requirements:
           - key: karpenter.k8s.gcp/instance-local-ssd-count
             operator: Exists
   ```

2. **The count must resolve to exactly one value.** Before launching a configurable machine, the combined NodePool and Pod requirements must pin the label to a single count with `In ["N"]`. An `Exists`, a multi-value `In`, or a `Gt` requirement leaves the count ambiguous, and the launch is rejected (see [Troubleshooting](#troubleshooting)). A Pod picks the count by adding the label to its node selector:

   ```yaml
   # Pod
   spec:
     nodeSelector:
       node.kubernetes.io/instance-type: n2d-standard-8
       karpenter.k8s.gcp/instance-local-ssd-count: "4"
   ```

The supported non-zero counts depend on the family and the machine's vCPU count:

| Family | Supported non-zero counts      |
|--------|--------------------------------|
| `n1`   | 1, 2, 3, 4, 5, 6, 7, 8, 16, 24 |
| `n2`   | up to 1, 2, 4, 8, 16, 24       |
| `n2d`  | up to 1, 2, 4, 8, 16, 24       |
| `c2`   | up to 1, 2, 4, 8               |
| `c2d`  | up to 1, 2, 4, 8               |

Larger machine types within a family support only the higher counts. Karpenter advertises only the counts a given machine type actually supports, and GCE offering availability in the target zone further constrains what launches. If no supported variant satisfies the request, no node is provisioned.

### Fixed-count machine types

Newer local SSD machine types — such as `c4d-standard-8-lssd` and `z3-highmem-8-highlssd` — bundle a fixed number of local SSDs into the machine type itself. For these types the count is read from GCE and is not configurable. Selecting the machine type is enough; you do not add the `karpenter.k8s.gcp/instance-local-ssd-count` label, and the exact-count rules above do not apply.

```yaml
# Pod
spec:
  nodeSelector:
    node.kubernetes.io/instance-type: c4d-standard-8-lssd
```

## Example

A count-enabled NodePool paired with a NodeClass that exposes local SSDs as ephemeral storage:

```yaml
apiVersion: karpenter.k8s.gcp/v1alpha1
kind: GCENodeClass
metadata:
  name: local-ssd
spec:
  localSsdMode: Ephemeral
  imageSelectorTerms:
    - family: ContainerOptimizedOS
      channel: cluster
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: local-ssd
spec:
  template:
    spec:
      nodeClassRef:
        group: karpenter.k8s.gcp
        kind: GCENodeClass
        name: local-ssd
      requirements:
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["on-demand"]
        - key: karpenter.k8s.gcp/instance-local-ssd-count
          operator: Exists
        - key: kubernetes.io/arch
          operator: In
          values: ["amd64"]
  disruption:
    consolidationPolicy: WhenEmpty
    consolidateAfter: 5m
```

A Pod that lands on this pool selects both the machine type and the count, and — in `Ephemeral` mode — can request the ephemeral storage it needs:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: cache
spec:
  nodeSelector:
    node.kubernetes.io/instance-type: n2d-standard-8
    karpenter.k8s.gcp/instance-local-ssd-count: "4"
  containers:
    - name: app
      resources:
        requests:
          ephemeral-storage: 800Gi  # capacity check in Ephemeral mode
```

The `ephemeral-storage` request is a capacity check against the storage the selected count provides — here four local SSDs, or 1500 GiB. It does not choose the count; the label does. The request only affects scheduling in `Ephemeral` mode, where local SSD capacity is advertised as `ephemeral-storage`.

## Migrating from `disks[].category: local-ssd`

Earlier versions accepted `local-ssd` as a `spec.disks[].category` value, but that shape treated local SSDs as persistent disks and never actually provisioned them. That value has been removed from the `GCENodeClass` schema and is now rejected. Remove any `category: local-ssd` disk entry from your NodeClasses and request local SSDs through `spec.localSsdMode` plus the count label instead.

## Troubleshooting

**A Pod requesting a configurable machine type never gets a node.** For the `n1`, `n2`, `n2d`, `c2`, and `c2d` families the count must resolve to exactly one value. If the requirements leave it ambiguous, Karpenter rejects the launch with an insufficient-capacity error and deletes the NodeClaim:

```
configurable local SSD launches require karpenter.k8s.gcp/instance-local-ssd-count to resolve to one exact count, or to be absent with a count=0 variant that fits the request
```

Confirm the NodePool includes the `karpenter.k8s.gcp/instance-local-ssd-count` requirement, and that the effective NodePool and Pod requirements pin it to a single count (`In ["N"]`) rather than `Exists`, a multi-value list, or `Gt`.

**A count-enabled NodePool still launches nodes without local SSDs.** A NodePool that omits the count requirement exposes only the count-`0` variant of configurable families, so no local SSDs are attached. Add the requirement shown above.

**`ephemeral-storage` requests are ignored.** Local SSD capacity backs `ephemeral-storage` only when `localSsdMode: Ephemeral`. In `RawBlock` mode the devices are raw and `ephemeral-storage` reflects the boot disk.

# Proposal: Local SSD Support for GCE NodeClasses

- **Status**: In Review
- **Authors**: @joemiller
- **Created**: 2026-06-14
- **Related Issues**: [#387](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/387), [#385](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/385)

---

## Summary

Karpenter-GCP should support GCE local SSDs in both GKE exposure modes: `RawBlock` and `Ephemeral`.

This proposal adds `GCENodeClass.spec.localSsdMode` and models local SSD count as a scheduler-visible instance type property. 1st/2nd generation machine types emit same-name variants per allowed count. 3rd/4th+ fixed-count local SSD SKUs emit one variant with the GCE-provided count.

The selected `InstanceType` variant is the source of truth for the physical attach count. Pod and NodePool requirements constrain the allowed set; they do not directly define the attach count.

---

## Motivation

### Problem Statement

The current `GCENodeClass.spec.disks[].category: local-ssd` shape is not a viable local SSD API:

- It models local SSDs as entries in the persistent disk list, but GCE local SSDs are SCRATCH disks.
- It does not express whether GKE should expose the SSDs as `Ephemeral` storage or `RawBlock` devices.
- It puts count on the NodeClass, which pushes users toward one NodeClass and one NodePool per count. ie: supporting the full range of 2nd gen n2(d) machine types would require many duplicated NodeClasses and NodePools per possible SSD count (0, 1, 2, 4, 8, 16, 24).

Karpenter-GCP also needs to represent local SSD capacity before launch. A single real GCE machine type such as `n2d-standard-8` can be created with 0, 1, 2, 4, 8, 16, or 24 local SSDs. In `Ephemeral` mode those shapes have different `ephemeral-storage` capacity. Treating all of those shapes as one `InstanceType` cannot correctly answer whether a pod requesting `800Gi` of `ephemeral-storage` fits.

### Goals

- Provision GCE nodes with local SSDs successfully.
- Support both `RawBlock` and `Ephemeral` exposure modes.
- Support 1st/2nd generation machine types without one NodeClass and NodePool per count.
- Support 3rd/4th+ generation fixed-count local SSD machine types.
- Allow RawBlock workloads to select an exact local SSD count.
- Allow Ephemeral workloads to schedule by normal `resources.requests.ephemeral-storage`.
- Provide a clear way for no-local-SSD NodePools to exclude local SSD variants.
- Preserve real GCE machine type names in `node.kubernetes.io/instance-type`.
- Keep create, returned NodeClaim, `List()`, `Get()`, drift, and consolidation behavior coherent for same-name variants.
- Fix z3 `OnHostMaintenance` behavior for local SSD variants.

### Non-Goals

- Persistent local SSD semantics. GCE local SSDs remain ephemeral.
- Per-pod local SSD mode. Mode is selected by `GCENodeClass`.
- Per-pod allocation of individual RawBlock devices. The count label describes node shape, not device allocation. See [Future Work](#future-work).
- LVM, TopoLVM, dm-cache, filesystem slicing, or other storage management above GKE's Ephemeral bootstrap behavior.
- TPU-specific scheduling semantics.

---

## Proposal

### Overview

Before:

```yaml
apiVersion: karpenter.k8s.gcp/v1alpha1
kind: GCENodeClass
spec:
  disks:
  - category: local-ssd
```

After:

```yaml
apiVersion: karpenter.k8s.gcp/v1alpha1
kind: GCENodeClass
spec:
  localSsdMode: RawBlock # or Ephemeral
```

Local SSD count is represented as a scheduler-visible instance type property:

```yaml
- key: karpenter.k8s.gcp/instance-local-ssd-count
  operator: In
  values: ["0", "1", "2", "4"]
```

The provider emits one `InstanceType` variant per count while keeping the real GCE machine type name:

```text
n2d-standard-8, count=0
n2d-standard-8, count=1
n2d-standard-8, count=2
n2d-standard-8, count=4
```

Each variant has:

```text
node.kubernetes.io/instance-type = n2d-standard-8
karpenter.k8s.gcp/instance-local-ssd-count In ["<count>"]
```

### API Changes

#### `GCENodeClass.spec.localSsdMode`

Add an optional field:

```yaml
spec:
  localSsdMode: RawBlock # or Ephemeral
```

Allowed values:

- `RawBlock`
- `Ephemeral`

Default: `RawBlock`.

The field controls how GKE bootstrap metadata exposes local SSDs when the selected count is greater than zero. It does not select the count. For count `0`, `localSsdMode` has no effect and no local SSD metadata is written.

`RawBlock` is the safer default because it does not make positive-count local SSD nodes advertise large Kubernetes `ephemeral-storage` capacity unless the user explicitly opts into that behavior.

Because `localSsdMode` changes bootstrap metadata and advertised capacity, the field is included in `GCENodeClass.Hash()`, and `GCENodeClassHashVersion` is bumped to `v4` so existing nodes drift when the effective mode changes.

#### `karpenter.k8s.gcp/instance-local-ssd-count`

Add a well-known label:

```text
karpenter.k8s.gcp/instance-local-ssd-count
```

The count value is used in four places:

- `InstanceType.Requirements`: every emitted variant has exactly one count value.
- Pod or NodePool requirements: users can constrain the allowed count set.
- GCE instance labels: the provider writes the selected count after create so `List()` / `Get()` can resolve same-name variants.
- Node / NodeClaim labels: the provider reports the selected count.

#### `GCENodeClass.spec.disks[].category: local-ssd`

Remove this shape. It is not a good scheduling API because it binds count to NodeClass, cannot represent 3rd/4th+ generation fixed counts cleanly, and does not express local SSD mode.

The value is dropped from the `DiskCategory` CRD enum, so it is rejected at admission. The disk renderer additionally skips any residual `local-ssd` entry on a stale in-memory object so it never renders as a (GCE-rejected) persistent disk.

### Instance Type Modeling

For 1st/2nd generation machine types, emit:

```text
{0} union AllowedLocalSSDCounts(machineTypeName, vCPUs)
```

Example for `n2d-standard-8`:

```text
n2d-standard-8, count=0
n2d-standard-8, count=1
n2d-standard-8, count=2
n2d-standard-8, count=4
n2d-standard-8, count=8
n2d-standard-8, count=16
n2d-standard-8, count=24
```

For 3rd/4th+ generation fixed-count local SSD machine types, emit one variant with the GCE-provided fixed count (counts below are illustrative; the real value comes from `MachineType.BundledLocalSsds.PartitionCount`):

```text
c4d-standard-8-lssd, count=1
z3-highmem-88-standardlssd, count=6
```

For machine types without local SSD support, emit one variant with count `0`.

Detection uses `MachineType.BundledLocalSsds` only. Nil means no local SSDs and emits count `0`. A positive `PartitionCount` emits one fixed-count variant. A present but missing or non-positive count skips the SKU rather than emitting a false count `0` variant. There is no suffix-based fallback.

3rd/4th+ generation fixed-count local SSD SKUs do not have a count `0` variant. A broad NodePool that allows these SKUs can therefore create a local SSD node for a pod that did not explicitly ask for SSDs. Operators who want a no-local-SSD pool should set `karpenter.k8s.gcp/instance-local-ssd-count In ["0"]` or otherwise exclude fixed-count local SSD SKUs.

### Capacity

Capacity is mode-aware.

In `RawBlock` mode, local SSDs are exposed as raw devices and should not be counted as Kubernetes `ephemeral-storage`. The instance type's `ephemeral-storage` capacity is based on the boot disk path.

In `Ephemeral` mode, local SSDs are configured as kubelet/container runtime ephemeral storage. Each count variant advertises the corresponding `ephemeral-storage` capacity. The boot disk should not be added on top of local SSD capacity for non-zero Ephemeral variants, because GKE mounts the local SSD backed filesystem for kubelet/container runtime storage.

```text
n2d-standard-8, count=0 -> boot disk ephemeral capacity
n2d-standard-8, count=1 -> about 375 GiB local SSD ephemeral capacity
n2d-standard-8, count=4 -> about 1500 GiB local SSD ephemeral capacity
z3-highmem-88-standardlssd, count=6 -> about 18000 GiB local SSD ephemeral capacity
```

This is the main reason to use same-name per-count variants. A single `InstanceType` with a multi-valued count requirement cannot truthfully advertise all possible Ephemeral capacities at once.

### Scheduling Contract

RawBlock workloads generally need an exact count because Kubernetes does not allocate individual raw NVMe devices from a count label:

```yaml
nodeSelectorTerms:
- matchExpressions:
  - key: karpenter.sh/nodepool
    operator: In
    values: ["n2d-rawblock"]
  - key: karpenter.k8s.gcp/instance-local-ssd-count
    operator: In
    values: ["4"]
```

Ephemeral workloads can schedule by normal Kubernetes resource requests:

```yaml
nodeSelectorTerms:
- matchExpressions:
  - key: karpenter.sh/nodepool
    operator: In
    values: ["n2d-ephemeral"]
containers:
- name: app
  resources:
    requests:
      ephemeral-storage: 800Gi
```

If exact disk count matters for an Ephemeral workload, the pod can also pin the count:

```yaml
nodeSelectorTerms:
- matchExpressions:
  - key: karpenter.sh/nodepool
    operator: In
    values: ["n2d-ephemeral"]
  - key: karpenter.k8s.gcp/instance-local-ssd-count
    operator: In
    values: ["4"]
containers:
- name: app
  resources:
    requests:
      ephemeral-storage: 800Gi
```

NodePool requirements can constrain the allowed count set for all pods in that pool:

```yaml
requirements:
- key: karpenter.k8s.gcp/instance-family
  operator: In
  values: ["n2d"]
- key: karpenter.k8s.gcp/instance-size
  operator: In
  values: ["8"]
- key: karpenter.k8s.gcp/instance-local-ssd-count
  operator: In
  values: ["0", "1", "2", "4"]
```

The NodeClaim may carry a multi-valued count requirement. That is valid. It represents the allowed count set, not the concrete count to attach. The concrete count is determined by the provider after it re-applies compatibility and resource-fit checks to the candidate variants.

### Variant Selection and Pricing

Same-name count variants should use uniform base machine price. If count `0` is cheaper than count `4`, consolidation can incorrectly see a valid count `4` node as replaceable by a cheaper count `0` sibling with the same `node.kubernetes.io/instance-type`.

Provider launch ordering should be deterministic after the provider filters candidate variants by:

- NodeClaim requirements
- compatible available offerings
- `resources.Fits(nodeClaim.Spec.Resources.Requests, instanceType.Allocatable())`

This provider-side resource fit is required. Karpenter core may carry the same real machine type name on the NodeClaim while leaving a multi-valued local SSD count requirement, so the provider must not assume that core handed it a single already-selected count variant.

Ordering after that filtering is:

1. price
2. real instance type name
3. local SSD count ascending

Consequences:

- For same-name variants with uniform price, a pod with no local SSD selector and no Ephemeral storage pressure picks count `0` first.
- An Ephemeral pod with a large `ephemeral-storage` request only sees variants that fit, then picks the smallest sufficient count.
- A NodePool that excludes count `0` intentionally defaults pods to the smallest allowed positive count.
- Uniform same-name pricing avoids count-based consolidation churn.

The count-ascending tie-break is a correctness invariant for 1st/2nd generation same-name variants. If it regresses, pods without local SSD requirements can receive positive-count SSD nodes.

### Count Resolution at Create

Provider `Create()` must resolve physical local SSD count from the selected candidate `InstanceType` variant.

```text
if the candidate InstanceType variant has exactly one local SSD count:
  attach / expose that count

if the candidate InstanceType variant has no local SSD count:
  fail clearly; this is a provider bug

if the NodeClaim count requirement has multiple values:
  allow it; it is the allowed set, not the concrete attach count
```

Do not resolve create-time count by parsing a single value from the NodeClaim's count requirement. That would make valid allowed sets such as `In ["1","4"]` fail even though the provider is already launching one concrete variant.

For Ephemeral capacity-only scheduling, `Create()` must choose from the provider-filtered variants, not from all variants that merely match the real machine type name. Otherwise a pod that requested `800Gi` of `ephemeral-storage` could be launched on the count `0` sibling even though the count `4` sibling was the first variant that fit.

### Returned NodeClaim and Reconciliation

Same-name variants make name-only instance type lookup unsafe. `Create()`, `List()`, and `Get()` must reconstruct the NodeClaim from the same NodeClass-mode-aware variant set, then select the variant by real machine type name plus selected local SSD count.

The provider writes the selected local SSD count as a GCE instance label at create time. `List()` / `Get()` reconstruction reads that GCE label and matches the same-name variant whose `karpenter.k8s.gcp/instance-local-ssd-count` requirement has the same count.

The immediate `Create()` return path should either use the selected candidate variant directly or use the same name-plus-count matching rule. Returned NodeClaim labels, capacity, and allocatable must match the resolved variant, not an arbitrary same-name sibling.

### Disk Attach and Metadata

For 1st/2nd generation machine types, append SCRATCH NVMe disks to the GCE instance create request according to the selected variant count.

For 3rd/4th+ generation fixed-count local SSD machine types, do not append explicit SCRATCH disks for the fixed local SSDs. The selected machine type already implies the local SSDs, and GCE attaches them according to the machine type definition.

When selected count is greater than zero, patch GKE bootstrap metadata according to `localSsdMode`:

| Mode        | Metadata                                              | Kube label                                              |
|-------------|-------------------------------------------------------|---------------------------------------------------------|
| `RawBlock`  | `NODE_LOCAL_NVME_SSD_BLOCK_EXT: "<count>,nvme,block"` | `cloud.google.com/gke-local-nvme-ssd=true`              |
| `Ephemeral` | `NODE_EPHEMERAL_STORAGE_LOCAL_SSD: "true"`            | `cloud.google.com/gke-ephemeral-storage-local-ssd=true` |

When selected count is `0`, local SSD metadata patching is a no-op.

### Unavailable Offerings

This proposal treats capacity failures as real machine type failures at the offering level. If `UnavailableOfferings` remains keyed by:

```text
capacityType:instanceType:zone
```

that is acceptable. A failed create for `n2d-standard-8` with count `16` can conservatively suppress `n2d-standard-8` in that zone for count `0`, `2`, and `4` as well. This may reduce scheduling opportunity during the cache TTL, but it does not create invalid node shapes or permit a workload to land on an unrequested SSD count.

This proposal intentionally does not require count-specific unavailable-offering keys. GCE capacity errors are not precise enough to reliably distinguish machine-type stockout from local-SSD-count stockout.

### z3 `OnHostMaintenance`

z3 local SSD SKUs have non-uniform maintenance requirements. The provider should select:

- `TERMINATE` for z3 bare-metal SKUs.
- `TERMINATE` for z3 non-metal SKUs whose fixed local SSD total exceeds 18 TiB.
- `MIGRATE` for smaller z3 non-metal SKUs.

The total should be derived from fixed local SSD partition count and partition size instead of hardcoding individual SKU names.

The implementation applies this in the broader maintenance policy order: spot/GPU terminate first; z3 non-metal follows the threshold rule; bare metal and h4d terminate; all other cases use the GCE default.

### Karpenter Core Caveat

Karpenter core has code paths that treat `InstanceType.Name` as unique. Same-name variants are therefore a deliberate provider pattern that requires tight invariants:

- same-name variants use the same real GCE machine type name
- same-name variants use the same offerings
- same-name variants use uniform base price
- each variant has exactly one count requirement
- the selected count is written to the GCE instance labels
- provider `Create()` return and `List()` / `Get()` reconstruction resolve by name plus count
- disruption, consolidation, and drift tests cover same-name variants

Sorting alone is not correctness. The selected shape must survive through create, returned NodeClaim status, and reconciliation.

---

## Risks and Mitigations

| Risk                                                                                          | Likelihood | Impact                                                                   | Mitigation                                                                                                                                         |
|-----------------------------------------------------------------------------------------------|------------|--------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------|
| Same-name variants interact badly with Karpenter code that assumes unique instance type names | Medium     | Incorrect drift, disruption, or returned capacity decisions              | Keep same-name variants constrained and test create, drift, disruption, and consolidation                                                          |
| Provider resolves attach count from NodeClaim requirement instead of selected variant         | Medium     | Multi-valued allowed sets fail or attach the wrong count                 | Make selected variant the source of truth                                                                                                          |
| Provider omits resource-fit filtering during create                                           | Medium     | Ephemeral capacity-only pods can launch on count `0` variants            | Re-apply `resources.Fits` before launch ordering and test capacity-only Ephemeral scheduling                                                       |
| Count-ascending tie-break regresses for same-name variants                                    | Low        | Pods without local SSD requirements can receive positive-count SSD nodes | Treat price/name/count ordering as a tested invariant                                                                                              |
| Broad NodePools include 3rd/4th+ fixed-count local SSD SKUs                                   | Medium     | SSD-indifferent pods can receive fixed-count local SSD nodes             | Use count `0` requirements for no-local-SSD pools or exclude fixed-count local SSD families                                                        |
| Uniform same-name pricing under-represents true local SSD cost                                | Medium     | Cost estimates are less precise                                          | Prefer scheduler correctness and consolidation stability; revisit count-specific pricing only if variant identity is safe                          |
| RawBlock count label is mistaken for per-pod device allocation                                | Medium     | Multiple pods can schedule to a node without actual device allocation    | Document that the label is node shape only; use a device plugin or storage layer for per-pod RawBlock allocation (see [Future Work](#future-work)) |

---

## Test Plan

### Unit Tests

- Variant emission for 1st/2nd generation, 3rd/4th+ fixed-count, and no-SSD machine types.
- Mode-aware capacity: Ephemeral advertises local SSD capacity; RawBlock does not.
- Provider-side filtering re-applies resource fit before ordering.
- Ordering: no-count pods choose count `0`; Ephemeral capacity-only pods choose the smallest sufficient count.
- Multi-valued NodeClaim count requirements are allowed sets; selected variant determines concrete count.
- Create attaches SCRATCH disks only for 1st/2nd generation machine types.
- Create writes the selected count as a GCE instance label.
- Returned NodeClaim and `List()` / `Get()` reconstruction match by name plus the GCE count label.
- Machine-type-level unavailable offering suppresses same-name count variants in the zone.
- z3 `OnHostMaintenance` policy covers bare-metal, non-metal over 18 TiB, and smaller non-metal SKUs.

### E2E / Integration Tests

- 1st/2nd generation count `0`, RawBlock exact-count, and Ephemeral capacity-only provisioning. Ephemeral exact-count uses the same count-label filtering as RawBlock plus the same metadata path as Ephemeral capacity-only, so unit coverage is sufficient.
- 3rd/4th+ fixed-count local SSD SKU in RawBlock and Ephemeral mode.
- A count `4` workload is not consolidated to a count `0` node.
- A no-local-SSD NodePool excludes 3rd/4th+ fixed-count local SSD SKUs with a count `0` requirement.
- At least one RawBlock and one Ephemeral local SSD e2e should run on both COS and Ubuntu, confirming GKE's per-OS bootstrapper honors the emitted kube-env keys.

---

## Acceptance Criteria

The feature is complete when:

- [ ] `GCENodeClass.spec.localSsdMode` exists with validation for `RawBlock` and `Ephemeral`, defaulting to `RawBlock`.
- [ ] `GCENodeClass.Hash()` includes `localSsdMode`; `GCENodeClassHashVersion` is bumped to `v4`.
- [ ] `karpenter.k8s.gcp/instance-local-ssd-count` is registered as a well-known provider label.
- [ ] 1st/2nd generation machine types emit same-name per-count variants.
- [ ] 3rd/4th+ generation fixed-count local SSD machine types emit one fixed-count variant.
- [ ] Ephemeral capacity is advertised per count variant.
- [ ] RawBlock local SSDs are not advertised as Kubernetes `ephemeral-storage`.
- [ ] Provider create uses selected candidate variant count, not a single value parsed from NodeClaim requirements.
- [ ] Provider writes selected count to GCE instance labels and reconstructs live instances by name plus count.
- [ ] Provider-side resource-fit filtering, uniform same-name variant pricing, and count tie-break avoid no-SSD pods selecting positive counts for 1st/2nd generation same-name variants.
- [ ] `disks[].category: local-ssd` is removed or rejected with a clear migration path.
- [ ] z3 `OnHostMaintenance` behavior is correct.
- [ ] Unit and e2e tests pass.

---

## Migration

Existing `GCENodeClass.spec.disks[].category: local-ssd` usage should move to the new API:

1. Set `spec.localSsdMode` on the `GCENodeClass`.
2. Remove `disks[].category: local-ssd` entries.
3. For RawBlock workloads, add a pod or NodePool requirement on `karpenter.k8s.gcp/instance-local-ssd-count`.
4. For Ephemeral workloads, add normal `resources.requests.ephemeral-storage`; add the count requirement only when exact SSD count matters.
5. For no-local-SSD pools that allow broad instance families, add `karpenter.k8s.gcp/instance-local-ssd-count In ["0"]` or otherwise exclude 3rd/4th+ generation fixed-count local SSD SKUs.

---

## Future Work

The count label is a provisioning signal: it selects node shape and, for 1st/2nd generation machine types, the create-time attach count. It does not advertise a Kubernetes resource that kube-scheduler allocates per pod, so it cannot prevent multiple pods that each want a raw device from landing on the same node.

Closing that gap with a k8s-native extended resource for RawBlock local SSDs is deferred. Such a resource would advertise per-node RawBlock capacity that kube-scheduler allocates per pod; it would not replace the label, since the GCE create API still needs an explicit attach count. Possible mechanisms, when needed: a Karpenter lifecycle hook that patches the Node with static capacity after boot (sufficient when nothing allocates in real time), or a device plugin (only if per-device allocation is required).

This keeps the model aligned with karpenter-aws, which provisions by node shape and accounts for shared local-disk capacity via NodeOverlay rather than a per-pod local-disk resource. If karpenter-aws gains richer local-disk primitives, this provider can follow.

## Alternatives Considered

### `GCENodeClass.spec.localSsdCount`

Putting both mode and count on `GCENodeClass` is the simplest model. It is also worse UX for 1st/2nd generation machine types because users need separate NodeClasses and NodePools for every count they want to allow. It also does not map well to 3rd/4th+ generation fixed-count local SSD SKUs where the machine type already determines count.

This remains a viable fallback if same-name variants prove unsafe, but it should not be the preferred design.

### Synthetic Instance Type Names

Example:

```text
n2d-standard-8-lssd4
```

Synthetic names avoid same-name variant identity issues, but they leak non-GCE names into `node.kubernetes.io/instance-type`, require translation back to real GCE machine types, and are harder to explain to users and contributors.

### One `InstanceType` Per Real Machine Type With Multi-Valued Count

Example:

```text
n2d-standard-8
karpenter.k8s.gcp/instance-local-ssd-count In ["0","1","2","4","8","16","24"]
```

This is attractive because it avoids same-name variants. It fails for Ephemeral capacity: one `InstanceType` cannot truthfully advertise boot-disk capacity, 375 GiB, 750 GiB, 1500 GiB, and larger capacities at the same time.

### Per-Count Pricing

Adding local SSD cost to each count variant would make no-SSD variants cheaper. It also risks consolidation replacing positive-count nodes with zero-count siblings because the real machine type name is the same. Uniform same-name variant pricing is safer until Karpenter core has explicit variant identity that makes count-specific pricing safe.

# Proposal: Local SSD Support for GCE NodeClasses

- **Status**: In Review
- **Authors**: @joemiller
- **Created**: 2026-06-14
- **Related Issues**: [#387](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/387), [#385](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/385)

---

## Summary

Karpenter-GCP should support GCE local SSDs in both GKE exposure modes: `RawBlock` and `Ephemeral`.

This proposal adds `GCENodeClass.spec.localSsdMode` and models local SSD count as a scheduler-visible instance type property. 1st/2nd generation machine types emit same-name variants per allowed count. 3rd/4th+ fixed-count local SSD SKUs emit one variant with the GCE-provided count.

For configurable 1st/2nd generation families, the effective Pod and NodePool requirements must resolve to one exact positive count before a new SSD-backed node can launch. The corresponding `InstanceType` variant remains the source of truth for physical attachment and capacity. Fixed-count 3rd/4th+ SKUs derive their count from the machine type.

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
- Require configurable-family RawBlock and Ephemeral workloads to select an exact local SSD count.
- Use normal `resources.requests.ephemeral-storage` as a capacity fit check for the selected Ephemeral shape.
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

Because `localSsdMode` changes bootstrap metadata and advertised capacity, the field is included in `GCENodeClass.Hash()`, and `GCENodeClassHashVersion` is bumped to `v5`. The version change prevents existing `v4` NodeClaims from drifting solely because `RawBlock` is newly defaulted; subsequent mode changes on `v5` NodeClasses trigger drift through the changed hash.

#### `karpenter.k8s.gcp/instance-local-ssd-count`

Add a well-known label:

```text
karpenter.k8s.gcp/instance-local-ssd-count
```

The count value is used in four places:

- `InstanceType.Requirements`: every emitted variant has exactly one count value.
- Pod or NodePool requirements: omitting the key selects configurable count `0`; once the NodePool declares the key, the effective NodePool and Pod requirements must resolve to one exact count.
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

For configurable 1st/2nd generation families, the NodePool count requirement controls which per-count variants Karpenter can consider. The effective NodePool and Pod requirements select the launch count.

| NodePool count requirement | Pod count selector | Configurable-machine result |
|----------------------------|--------------------|-----------------------------|
| omitted                    | omitted or `0`     | count `0`                   |
| omitted                    | positive count     | no matching variant         |
| `Exists`                   | omitted            | rejected; NodeClaim deleted |
| `Exists`                   | `0` or `4`         | selected count              |
| `In ["0","2","4"]`         | omitted            | rejected; NodeClaim deleted |
| `In ["0","2","4"]`         | `0` or `4`         | selected count              |
| `In ["4"]`                 | omitted or `4`     | count `4`                   |
| `Gt ["0"]`                 | omitted            | rejected; NodeClaim deleted |
| `Gt ["0"]`                 | `4`                | count `4`                   |

The result also depends on machine support, offering availability, and resource fit. A fixed-count bundled SKU is not subject to this table because its machine type already defines the count.

A NodePool can let Pods choose any supported count without listing the values:

```yaml
# NodePool spec.template.spec.requirements
- key: karpenter.k8s.gcp/instance-local-ssd-count
  operator: Exists
```

Every configurable-machine Pod on this pool selects a count. Count `0` is explicit:

```yaml
# Pod spec
nodeSelector:
  node.kubernetes.io/instance-type: n2-standard-2
  karpenter.k8s.gcp/instance-local-ssd-count: "0"
```

A Pod can select a positive count on the same pool:

```yaml
# Pod spec
nodeSelector:
  node.kubernetes.io/instance-type: n2d-standard-4
  karpenter.k8s.gcp/instance-local-ssd-count: "4"
```

This produces `n2d-standard-4` with count `4`. The same selectors may be expressed as match expressions in one required `nodeSelectorTerm`.

A positive-only pool requires each configurable-machine Pod to select one exact count:

```yaml
# NodePool spec.template.spec.requirements
- key: karpenter.k8s.gcp/instance-local-ssd-count
  operator: Gt
  values: ["0"]
```

A Pod without a count selector cannot launch a configurable machine on this pool. The same is true for any count-enabled NodePool with a non-singleton requirement. On a mixed pool that also allows configurable names, every non-DaemonSet Pod therefore pins either one exact count or one exact bundled machine type in the same selector term. A bundled-only pool may remain unpinned. Ordinary DaemonSets remain broad and do not need a count selector.

Ephemeral mode uses the same count-selection rules. The storage request checks the selected shape; it does not select or increase the count:

```yaml
# GCENodeClass spec
localSsdMode: Ephemeral
---
# Pod spec
nodeSelector:
  node.kubernetes.io/instance-type: n2d-standard-8
  karpenter.k8s.gcp/instance-local-ssd-count: "4"
containers:
- name: app
  resources:
    requests:
      ephemeral-storage: 800Gi
```

This Pod gets count `4` when that shape provides at least `800Gi` allocatable `ephemeral-storage`. If count `4` is too small, the Pod remains unschedulable. The provider never infers a count from the resource request.

### Variant Selection and Pricing

Same-name count variants should use uniform base machine price. If count `0` is cheaper than count `4`, consolidation can incorrectly see a valid count `4` node as replaceable by a cheaper count `0` sibling with the same `node.kubernetes.io/instance-type`.

Provider launch ordering should be deterministic after the provider filters candidate variants by:

- NodeClaim requirements
- compatible available offerings
- `resources.Fits(nodeClaim.Spec.Resources.Requests, instanceType.Allocatable())`

Provider-side resource fit remains required as defense in depth. A configurable launch from a count-enabled NodePool additionally requires the NodeClaim count to be one exact value. Count `0` is implicit only when the NodePool omits the count key.

Ordering after that filtering is:

1. price
2. real instance type name
3. local SSD count ascending

Consequences:

- A NodePool without the count key exposes only configurable count `0`, including for DaemonSet overhead calculations.
- An exact configurable count selects one same-name variant; an Ephemeral request must fit that variant.
- A broad count requirement can still select a compatible fixed-count bundled SKU, but cannot launch a configurable machine.
- Uniform same-name pricing avoids count-based consolidation churn.

The count-ascending tie-break remains deterministic, but correctness for new configurable launches comes from exact-count filtering rather than choosing among ambiguous variants.

### Count Resolution at Create

Provider `Create()` validates the effective NodeClaim count before creating a new configurable-family VM, then resolves physical attachment from the selected candidate variant.

```text
if the configurable count requirement is absent:
  allow only count 0

if it is In with one non-negative integer:
  allow only that exact configurable variant

if it is broad, ranged, or multi-valued:
  exclude configurable variants
  retain compatible fixed-count fallbacks

if no fixed-count launch candidate remains:
  return an InsufficientCapacityError without marking offerings unavailable
  core deletes the invalid NodeClaim immediately

for the selected candidate variant:
  require one concrete variant count and attach / expose that count
```

The new-launch gate runs after existing-instance lookup so a response-lost VM from an older broad NodeClaim can still be adopted. The ambiguous-count error is classified as insufficient capacity only to use core's immediate NodeClaim-deletion path; no offering is marked unavailable. This avoids an empty-providerID NodeClaim making `Cluster.Synced()` false for the five-minute launch timeout. Ephemeral capacity-only positive-count selection is intentionally unsupported.

Static NodePools have no Pod requirements to narrow a broad count. A static configurable NodePool must omit the count key for count0 or use singleton `In` for one exact count.

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

Core also counts `InstanceTypeOptions` rows for the 15-option spot-to-spot consolidation guard and truncates those rows to 600 before serializing unique machine names. For supported configurable launches and key-less pools, the contract prevents count variants from inflating those paths: key-less pools expose only count `0`, and count-enabled pools require one exact effective count. Fixed-count bundled SKUs already have one row per machine name. Operators must keep non-DaemonSet Pods without an exact count or exact bundled SKU off mixed count-enabled pools, including during disruption replacement.

---

## Risks and Mitigations

| Risk                                                                                          | Likelihood | Impact                                                                | Mitigation                                                                                                                                         |
|-----------------------------------------------------------------------------------------------|------------|-----------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------|
| Same-name variants interact badly with Karpenter code that assumes unique instance type names | Medium     | Incorrect drift, disruption, or returned capacity decisions           | Keep same-name variants constrained and test create, drift, disruption, and consolidation                                                          |
| Core drops a scheduler-narrowed same-name/count pairing at NodeClaim serialization            | Medium     | Provider can launch a configurable count core rejected                | Require one exact effective count for new configurable positive-SSD launches; expose only count0 on key-less pools                                 |
| Provider omits resource-fit filtering during create                                           | Medium     | A pinned Ephemeral shape can be too small for the workload            | Re-apply `resources.Fits` before launch ordering and test exact-count Ephemeral fit                                                                |
| Broad configurable count uses the insufficient-capacity lifecycle classification              | Medium     | Event/metric says capacity although the request is invalid            | Do not mark offerings unavailable; use the classification only so core immediately deletes the NodeClaim and does not stall all provisioning       |
| Broad NodePools include 3rd/4th+ fixed-count local SSD SKUs                                   | Medium     | SSD-indifferent pods can receive fixed-count local SSD nodes          | Use count `0` requirements for no-local-SSD pools or exclude fixed-count local SSD families                                                        |
| Uniform same-name pricing under-represents true local SSD cost                                | Medium     | Cost estimates are less precise                                       | Prefer scheduler correctness and consolidation stability; revisit count-specific pricing only if variant identity is safe                          |
| RawBlock count label is mistaken for per-pod device allocation                                | Medium     | Multiple pods can schedule to a node without actual device allocation | Document that the label is node shape only; use a device plugin or storage layer for per-pod RawBlock allocation (see [Future Work](#future-work)) |

---

## Test Plan

### Unit Tests

- Variant emission for 1st/2nd generation, 3rd/4th+ fixed-count, and no-SSD machine types.
- Mode-aware capacity: Ephemeral advertises local SSD capacity; RawBlock does not.
- Provider-side filtering re-applies resource fit before ordering.
- Key-less NodePools expose only configurable count `0`; count-declaring pools expose the per-count catalog.
- `Gt 0`, `Exists`, singleton `In`, and multi-value `In` NodePools intersect with exact Pod selectors to serialize one count, including explicit count0.
- Static configurable claims accept omitted or singleton counts and reject broad counts.
- Broad configurable NodeClaims return uncached `InsufficientCapacityError` with no insert; core's launch lifecycle deletes them immediately, while compatible bundled fallbacks remain eligible.
- Count0-selective DaemonSet overhead is accounted for on key-less pools.
- Existing positive-count VMs use the full reconstruction catalog and are adopted before new-launch validation.
- Create attaches SCRATCH disks only for 1st/2nd generation machine types.
- Create writes the selected count as a GCE instance label.
- Returned NodeClaim and `List()` / `Get()` reconstruction match by name plus the GCE count label.
- Machine-type-level unavailable offering suppresses same-name count variants in the zone.
- z3 `OnHostMaintenance` policy covers bare-metal, non-metal over 18 TiB, and smaller non-metal SKUs.

### E2E / Integration Tests

- 1st/2nd generation count `0`, RawBlock exact-count, and Ephemeral exact-count provisioning, including Ephemeral resource-fit validation.
- Broad `Gt 0`, `Exists`, singleton, and multi-value NodePool requirements with exact configurable Pod selection, including explicit count0.
- Static configurable NodePools with omitted and singleton count requirements.
- 3rd/4th+ fixed-count local SSD SKU in RawBlock and Ephemeral mode.
- A count `4` workload is not consolidated to a count `0` node.
- A no-local-SSD NodePool excludes 3rd/4th+ fixed-count local SSD SKUs with a count `0` requirement.
- At least one RawBlock and one Ephemeral local SSD e2e should run on both COS and Ubuntu, confirming GKE's per-OS bootstrapper honors the emitted kube-env keys.

---

## Acceptance Criteria

The feature is complete when:

- [ ] `GCENodeClass.spec.localSsdMode` exists with validation for `RawBlock` and `Ephemeral`, defaulting to `RawBlock`.
- [ ] `GCENodeClass.Hash()` includes `localSsdMode`; `GCENodeClassHashVersion` is bumped to `v5`.
- [ ] `karpenter.k8s.gcp/instance-local-ssd-count` is registered as a well-known provider label.
- [ ] 1st/2nd generation machine types emit same-name per-count variants.
- [ ] 3rd/4th+ generation fixed-count local SSD machine types emit one fixed-count variant.
- [ ] Ephemeral capacity is advertised per count variant.
- [ ] RawBlock local SSDs are not advertised as Kubernetes `ephemeral-storage`.
- [ ] Provider allows implicit configurable count0 only when the NodePool omits the count key, otherwise requires one exact effective count before launch, then attaches the selected candidate variant count.
- [ ] Provider writes selected count to GCE instance labels and reconstructs live instances by name plus count.
- [ ] Key-less count0 scheduling, exact-count launch filtering, provider-side resource fit, and uniform same-name pricing prevent selection of an unstated configurable positive count.
- [ ] `disks[].category: local-ssd` is removed or rejected with a clear migration path.
- [ ] z3 `OnHostMaintenance` behavior is correct.
- [ ] Unit and e2e tests pass.

---

## Migration

Existing `GCENodeClass.spec.disks[].category: local-ssd` usage should move to the new API:

1. Set `spec.localSsdMode` on the `GCENodeClass`.
2. Remove `disks[].category: local-ssd` entries.
3. For configurable-family RawBlock and Ephemeral SSD workloads, either omit the NodePool count key for implicit count0 or ensure the effective Pod/NodePool requirement resolves to one exact count, including explicit count0 on count-enabled pools.
4. For Ephemeral workloads, also add normal `resources.requests.ephemeral-storage` as a fit check for the selected count.
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

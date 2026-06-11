# Proposal: Local-SSD Support for GCE NodeClasses

- **Status**: Draft
- **Authors**: @joemiller
- **Created**: 2026-05-26
- **Related Issues**: [#385](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/385)

---

## Summary

Karpenter cannot currently provision GCE nodes with local SSDs. The existing `disks[].category: local-ssd` shape on `GCENodeClass` produces an API-level GCE rejection (`Cannot create local SSD as persistent disk`). It also does not distinguish GKE's two local-SSD exposure modes (Ephemeral vs RawBlock), and forces a 1:1 split of NodeClass/NodePool per SSD count when an operator wants mixed counts in a single pool.

This proposal replaces that shape with a per-pod count label (`karpenter.k8s.gcp/instance-local-ssd-count`) plus a NodeClass-level mode (`spec.localSsdMode: Ephemeral | RawBlock`, default `Ephemeral`). Local SSDs are attached as SCRATCH NVMe at instance create, and `kube-env` is patched with the same keys GKE-native node pools set, so the in-tree GKE bootstrap scripts handle format / RAID / mount unchanged.

Configurable families (`n1`, `n2`, `n2d`, `c2`, `c2d`) emit one InstanceType variant per allowed SSD count. Bundled-SSD SKUs (`c4*-lssd`, `c4a-*-lssd`, `c4d-*-lssd`, `h4d-*-lssd`, `z3-*-{standard,high}lssd`, `a2-ultragpu-*`, `a3-*`, `a4*-*`) pin the count from the machine type.

The proposal also fixes `Scheduling.OnHostMaintenance` for z3 SKUs. GCE requires `TERMINATE` on bare-metal and on > 18 TiB non-metal variants, but `MIGRATE` on smaller non-metal variants.

---

## Motivation

### Problem Statement

GCE local SSDs are attached as SCRATCH NVMe disks, distinct from persistent disks. They ship on two kinds of machine type: **configurable families** (`n1`/`n2`/`n2d`/`c2`/`c2d`) where the operator picks the count, and **bundled-SSD SKUs** where the count is fixed by the machine type (`c4*-lssd`, `c4a-*-lssd`, `c4d-*-lssd`, `h4d-*-lssd`, the GPU families `a2-ultragpu-*` / `a3-*` / `a4-*` / `a4x-*`, and the storage-optimized `z3-*-{standard,high}lssd` family). Families that ship both bundled and non-bundled variants (e.g. `c4d-*-lssd` / `c4d-*-nolssd`, `a3-*` / `a3-*-nolssd`) are distinguished by name suffix — see the `-nolssd` rule in Design Details. z3 is GCE's local-SSD-first SKU line, with 3000 GiB partitions instead of the usual 375 GiB. GKE then exposes the attached SSDs in one of two modes:

- **RawBlock** — raw NVMe block devices the workload formats and mounts itself. Triggered by `NODE_LOCAL_SSDS_EXT: "<count>,nvme,block"` in `kube-env` and the `cloud.google.com/gke-local-nvme-ssd=true` kube-label.
- **Ephemeral** — GKE's startup script RAID-0s the SSDs, formats ext4, mounts at `/mnt/stateful_partition/var/lib/containerd` etc., and reports the capacity as `ephemeral-storage`. Triggered by `NODE_LOCAL_SSDS_EPHEMERAL: "true"` plus the `cloud.google.com/gke-ephemeral-storage-local-ssd=true` kube-label.

The existing provider has three concrete issues:

1. **Persistent-disk error.** Setting `disks[N].category: local-ssd` is translated to a persistent-disk attach in `renderDiskProperties`, which GCE rejects with `Invalid value for field 'resource.disks[N].type': 'PERSISTENT'. Cannot create local SSD as persistent disk.` No node is ever produced.
2. **No mode distinction.** Neither the API surface nor the metadata patching path knows about Ephemeral vs RawBlock. Operators cannot pick.
3. **Count proliferation.** Because the count lives in `spec.disks[]`, an operator who wants 1, 2, and 4-SSD nodes in the same pool has to maintain three near-identical NodeClass + NodePool pairs.

A secondary bug: z3 SKUs have non-uniform `OnHostMaintenance` requirements (`TERMINATE` for bare-metal and for non-metal > 18 TiB, `MIGRATE` otherwise). The current code picks one default and is rejected by GCE on the other half of the family.

### Goals

- Provisioning succeeds for both configurable and bundled-SSD machine types.
- Operators can pick `Ephemeral` or `RawBlock` per NodeClass.
- One NodeClass + NodePool can serve pods with different SSD counts and the no-SSD case.
- Scheduling by `requests.ephemeral-storage` works in `Ephemeral` mode without per-pod count math.
- z3 SKUs schedule and bootstrap with the correct `OnHostMaintenance`.
- The `disks[].category: local-ssd` shape is removed from the CRD enum, so `kubectl apply` rejects it at admission rather than failing later at instance creation. Since GCE currently rejects every attempt to use it, no working deployment depends on it; a migration note covers the transition.

### Non-Goals

- Persistent local-SSD semantics (GCE local SSDs are inherently ephemeral; survive only soft reboots).
- LVM, dm-cache, or other in-node assemblies beyond what GKE's bootstrap scripts already do for Ephemeral mode.
- TPU pods (a3-mega and related SKUs that bundle TPUs alongside local SSDs are out of scope for this proposal even though the SSD-count emission would work for them).

---

## Proposal

### Overview

```
Before:
  GCENodeClass.spec.disks: [{ category: local-ssd, ... }, ...]
    → GCE: "Cannot create local SSD as persistent disk"

After:
  GCENodeClass.spec.localSsdMode: Ephemeral | RawBlock   (default Ephemeral)
  pod.spec.nodeSelector:
    karpenter.k8s.gcp/instance-local-ssd-count: "<N>"   (configurable families)
    node.kubernetes.io/instance-type: <bundled-sku>      (bundled-SSD SKUs)
    → SCRATCH NVMe attached, kube-env patched, GKE bootstrap mounts as configured.
```

### API Changes

**`GCENodeClass.spec.localSsdMode`** — new optional field, enum `Ephemeral | RawBlock`, default `Ephemeral`. Set once per NodeClass. Controls only the mode; counts come from the scheduler. `Ephemeral` is the default to match GKE's recommended local-SSD path (the `emptyDir`/ephemeral-storage usage GKE docs lead with) and so pods can schedule by `requests.ephemeral-storage` with no extra configuration. GKE itself forces an explicit choice (`--ephemeral-storage-local-ssd` vs `--local-nvme-ssd-block`); we pick the more common one as the default. `RawBlock` is opt-in for workloads that format and mount the raw NVMe devices themselves.

**`karpenter.k8s.gcp/instance-local-ssd-count`** — new well-known label key. Three roles:

1. Pod-side selector on configurable families (`n1`/`n2`/`n2d`/`c2`/`c2d`): selects an InstanceType variant with that many SSDs. Operators can express this via `pod.spec.nodeSelector` or `pod.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution`; Karpenter normalizes both into the same scheduling requirement before resolution.
2. Node label set at provisioning so workloads / DaemonSets can target by SSD count.
3. NodePool `requirements` entry: pin the count at the pool level when no pod-side label is set.

The key is registered in `v1alpha1.WellKnownLabels` so Karpenter's scheduler treats it as a managed label.

**`GCENodeClass.spec.disks[].category = "local-ssd"`** — removed. The value is dropped from the `Disk.Category` CRD enum, so the apiserver rejects any NodeClass that sets it at admission time rather than failing later at instance creation. This shape never produced a working node — GCE rejects the persistent-disk attach outright — so no functioning deployment depends on it. The migration note below covers any stored object that still carries the value.

### Design Details

#### InstanceType variant emission

`pkg/providers/instancetype/types.go` learns to emit the SSD-count requirement per variant:

- **Bundled-SSD SKUs** (detected via `MachineType.BundledLocalSsds.PartitionCount` from the GCE API, with a name-suffix/-prefix fallback for cache-miss): one variant, count requirement pinned to the bundled value. Names ending in `-nolssd` (e.g. `c4d-highmem-8-nolssd`, `a3-ultragpu-8g-nolssd`) override the family-level bundled classification — they are treated as non-bundled with count pinned to `0`, so the prefix-based GPU-family match (`a3-`, `a4-`, etc.) does not over-promote them. The override applies to the name-fallback path only; when the GCE API is the source of truth, `BundledLocalSsds.PartitionCount` is already correct.
- **Configurable-SSD families**: one variant per count returned by `AllowedLocalSSDCounts(machineName, vCPUs)`, plus a zero-SSD variant for pods that don't request any SSDs. (e.g. for `n2d-standard-8` the allowed counts are 1, 2, 4, 8, 16, 24, so 7 variants including zero.)
- **No-SSD families** (e.g. `e2`): one variant, count requirement pinned to `0`.

In `Ephemeral` mode, each non-zero variant's `ResourceEphemeralStorage` is set to the total local-SSD capacity (`partitionCount × 375 GiB` for standard families, `× 3000 GiB` for z3, with per-SKU overrides for machines where the API reports a wrong partition count). The boot-disk bytes are not added — GKE's bootstrap script mounts the SSD-backed RAID-0 volume as the kubelet's ephemeral-storage filesystem, so the boot disk is excluded from the advertised capacity. In `RawBlock` mode (and the zero-SSD variant) `ResourceEphemeralStorage` falls back to the boot-disk size; local SSDs are exposed as raw NVMe devices the workload manages itself. This is what makes scheduling by `requests.ephemeral-storage` work in Ephemeral mode without an explicit count: Karpenter picks the smallest variant whose ephemeral-storage capacity satisfies the request.

#### Variant selection & scheduler preference

Emitting a zero-SSD variant plus one variant per allowed count raises a correctness question: if a pod requests no local SSD, every variant satisfies it, so what stops Karpenter from picking the 24-SSD variant and attaching (and billing for) SSDs nothing uses?

The intuitive fix is to price each variant by its SSD count so the zero-SSD variant is the cheapest. This does not work. Once variants of one machine type carry different prices, Karpenter's consolidation controller reads a correctly-provisioned SSD node as over-priced relative to the zero-SSD variant and churns it, replacing running nodes in a loop. (We tried per-variant pricing and observed exactly this in mixed-pool e2e.)

Instead, all variants of a machine type keep a uniform price, and `orderInstanceTypesByPrice` (`pkg/providers/instance/instance.go`) breaks ties deterministically by ascending SSD count, after price and instance-type name. Consequences:

- A pod that requests no SSD count lands on the zero-SSD variant (smallest count wins the tie), so no SSDs are attached unless asked for.
- An Ephemeral capacity request picks the smallest-count variant whose `ResourceEphemeralStorage` satisfies the request, so SSDs are not over-provisioned.
- Uniform pricing means consolidation never sees an SSD node as more expensive than its zero-SSD sibling, so there is no replace loop.
- Bundled-SSD SKUs emit a single variant, so the tie-break is a no-op for them.

This makes the count label an escape hatch rather than the primary scheduling signal: in the default Ephemeral mode, capacity-based scheduling plus the deterministic tie-break already steer pods to the right variant without any pod-side label.

#### Unavailable-offering granularity

The `UnavailableOfferings` cache records offerings GCE recently rejected for capacity (`ZONE_RESOURCE_POOL_EXHAUSTED` etc.) so the scheduler skips them for a TTL (30 min). Its key is `(capacityType, machineType, zone)`. Count-variants share a machine-type name, so by default a capacity failure on any one variant marks every count of that machine type unavailable in that zone. That coarse key has a benign failure mode and a costly one:

- **Benign:** a no-SSD pod can never be pushed onto an SSD variant. The variants go unavailable together, and the pod always targets the smallest count first, so it can only ever fall through to a different machine type, never to a higher count of the same one.
- **Costly:** a count-specific failure (e.g. GCE cannot attach 24 local SSDs in a zone) blocks the 0/1/2-count variants too, for the full TTL, even though they would have succeeded.

The obvious fix — keying the cache by count — reintroduces the benign case as a real bug. A plain machine-type stockout is reported against whatever count was attempted, and GCE masks count-specific local-SSD exhaustion as the same generic `ZONE_RESOURCE_POOL_EXHAUSTED`, so a per-count key cannot tell the two apart. With independent per-count entries, a stockout recorded against the count=0 variant would leave the count=1 variant looking available, and a no-SSD pod would be provisioned onto it.

Instead the cache stores, per `(capacityType, machineType, zone)`, the **minimum failed SSD count**, and `IsUnavailable(machineType, zone, capacityType, variantCount)` returns true when an entry exists and `variantCount >= storedMin`. Suppression is monotone upward: a failure at count 24 suppresses 24 and above but leaves 0/1/2 available; a failure at count 0 suppresses everything. This is safe without distinguishing stockout from SSD-attach failure: a no-SSD pod attempts count 0 first, and for it to reach count 1 the count-0 offering would have to be unavailable while count 1 is available, which cannot happen because count-0 unavailability forces `storedMin = 0` and suppresses all counts. The costly case is fixed (low counts stay schedulable) while the benign guarantee is preserved structurally.

Implementation notes:

- The attempted count is threaded to both `MarkUnavailable` call sites in `pkg/providers/instance/instance.go` (the synchronous `Instances.Insert` failure and the async operation-error path through `waitOperationDone` / `handleZoneOperationError`).
- The minimum-count update is a read-modify-write, so it is guarded by a mutex on the `UnavailableOfferings` type to avoid a race that could leave `storedMin` above the true minimum (which would cost a redundant retry, not correctness).
- Spot-interruption marks, if reintroduced, record count 0 (a preempted instance is a machine-type-level signal, so it suppresses all counts).

#### Count resolution at provisioning

`resolveLocalSSDCount` (`pkg/providers/instance/instance.go`) picks the count to send to the GKE bootstrapper. Precedence:

1. `MachineType.BundledLocalSsds.PartitionCount`: authoritative for bundled SKUs. Wins unconditionally. If the cache doesn't have the MachineType for a name that looks bundled, the resolver returns a retryable error so the Create loop falls through rather than booting a node with the wrong physical SSD count.
2. The NodeClaim's `karpenter.k8s.gcp/instance-local-ssd-count` requirement (originating from `pod.spec.nodeSelector`, `pod.spec.affinity.nodeAffinity`, or `NodePool.spec.template.spec.requirements`; all three are normalized by Karpenter into the same `scheduling.Requirement` before reaching the resolver). Operator must be `In` with a single value. `NotIn` / `Gt` / `Lt` / multi-value `In` are surfaced as `CreateError` (scope cut: supporting them requires per-machine-type allowed-count metadata we don't carry).
3. Zero.

There is no legacy `disks[].category=local-ssd` fallback: the value is removed from the CRD enum (see API Changes), so the resolver never observes it.

#### Disk attach

`renderDiskProperties` (`pkg/providers/instance/instance.go`) appends SCRATCH NVMe entries to `disks[]` after the boot/data disks declared on the NodeClass — but **only for configurable families**, where GCE does not attach local SSDs unless they are declared. The count comes from the pod-requested value resolved by `resolveLocalSSDCount`. For bundled SKUs and the zero case it appends nothing.

For bundled-SSD SKUs (`c4*-lssd`, `c4a-*-lssd`, `c4d-*-lssd`, `h4d-*-lssd`, `z3-*-{standard,high}lssd`, `a2-ultragpu-*`, `a3-*`, `a4*-*`) GCE attaches the SSDs server-side from `MachineType.BundledLocalSsds.PartitionCount`, so we declare nothing in `disks[]` — matching gcloud's first-party body, which sends only the boot disk for these machines (`gcloud compute instances create --machine-type=<bundled-sku>`, verified with `--log-http`). Emitting redundant SCRATCH entries alongside a bundled machine is accepted by `instances.insert` on `c4*-lssd` / `c4a-*-lssd` / `c4d-*-lssd` / `z3-*-{standard,high}lssd` but is unverified on the GPU (`a2`/`a3`/`a4`) and `h4d` families; since the entries carry no benefit and a rejection would fail provisioning at create time on expensive, scarce hardware, we send exactly what gcloud sends and avoid the question entirely.

For bundled SKUs the count still flows from `resolveLocalSSDCount` into `PatchLocalSSDMetadata` so GKE's bootstrap script receives the correct `NODE_LOCAL_SSDS_*` count and formats / RAIDs the server-attached SSDs. The format / mount path depends on that metadata count, not on the `disks[]` entries, so omitting the SCRATCH declarations does not change how the SSDs are exposed.

The legacy `disks[].category: local-ssd` branch is removed (it was the source of the persistent-disk error).

#### Metadata patching

`PatchLocalSSDMetadata` (`pkg/providers/metadata/localssd.go`) upserts the kube-env and kube-labels entries GKE bootstraps from:

| mode        | kube-env                                    | kube-label                                              |
|-------------|---------------------------------------------|---------------------------------------------------------|
| `RawBlock`  | `NODE_LOCAL_SSDS_EXT: "<count>,nvme,block"` | `cloud.google.com/gke-local-nvme-ssd=true`              |
| `Ephemeral` | `NODE_LOCAL_SSDS_EPHEMERAL: "true"`         | `cloud.google.com/gke-ephemeral-storage-local-ssd=true` |

The function removes the opposite-mode entries on each call, so flipping `localSsdMode` on an existing NodeClass produces a clean single-mode state on the next provisioning cycle. It returns an error if `kube-env` or `kube-labels` is missing from the metadata items, consistent with the existing `PatchKubeEnvForInstanceType` and `AppendGPUTaint` patterns.

`PatchLocalSSDMetadata` is a no-op when count is zero (no kube-env or kube-labels entries are written), so the zero-SSD variant in a NodeClass with `localSsdMode: Ephemeral` never gets a stray `NODE_LOCAL_SSDS_EPHEMERAL: "true"` and bootstraps normally.

#### z3 `OnHostMaintenance` fix

`onHostMaintenancePolicy` (`pkg/providers/instance/instance.go`) returns:

- `TERMINATE` for all bare-metal z3 SKUs (suffix `-metal`).
- `TERMINATE` for non-metal z3 SKUs whose bundled SSD total exceeds 18 TiB (currently `z3-highmem-88-highlssd`, `z3-highmem-176-standardlssd`).
- `MIGRATE` for all other z3 SKUs.

The thresholds are derived from `MachineType.BundledLocalSsds.PartitionCount` × the partition size, not hardcoded SKU names, so future z3 variants don't need a code change.

#### Code changes

| File                                                 | Change                                                                                                        |
|------------------------------------------------------|---------------------------------------------------------------------------------------------------------------|
| `pkg/apis/v1alpha1/gcenodeclass.go`                  | New `LocalSsdMode` field + `LocalSSDMode` enum. Hash version bump to `v4`.                                    |
| `pkg/apis/v1alpha1/labels.go`                        | New `LabelInstanceLocalSsdCount`; registered in well-known labels.                                            |
| `pkg/utils/localssd/localssd.go`                     | New. `AllowedLocalSSDCounts`, `FamilySupportsConfigurableLocalSSDs`, `TotalGiB`.                              |
| `pkg/providers/instance/localssd.go`                 | New. Bundled-SKU detection.                                                                                   |
| `pkg/providers/instance/instance.go`                 | `resolveLocalSSDCount`, SCRATCH disk attach (configurable families only), z3-aware `onHostMaintenancePolicy`. |
| `pkg/providers/instancetype/{types,instancetype}.go` | Per-variant SSD-count requirement and ephemeral-storage capacity.                                             |
| `pkg/providers/metadata/`                            | New `localssd.go` (`PatchLocalSSDMetadata`) hooked into the existing metadata-patch pipeline in `utils.go`.   |

---

## Risks and Mitigations

| Risk                                                                                           | Likelihood | Impact                                                                                                   | Mitigation                                                                                                                                                                              |
|------------------------------------------------------------------------------------------------|------------|----------------------------------------------------------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Bundled-SKU MachineType cache miss → resolver can't read `BundledLocalSsds.PartitionCount`     | Low        | Metadata count would be wrong (or zero), so GKE wouldn't format / RAID the SSDs GCE attached server-side | Resolver returns a retryable error; Create loop falls through to next candidate instead of booting.                                                                                     |
| Operator uses an unsupported requirement operator (`NotIn`, `Gt`, etc.) on the SSD-count label | Medium     | Provisioning fails                                                                                       | Resolver surfaces a `CreateError` with a clear message naming the unsupported operator.                                                                                                 |
| GKE changes the kube-env / kube-label keys used by the bootstrap script                        | Low        | Local-SSDs wouldn't be formatted/mounted on Ephemeral; raw devices still appear on RawBlock              | Keys are the same ones GKE-native pools set; verified against live `kube-env` dumps. Same risk profile as the existing `PatchKubeEnvForInstanceType`.                                   |
| Existing deployments rely on `disks[].category=local-ssd`                                      | Low        | A NodeClass that sets it is rejected at `kubectl apply` once the enum value is removed                   | GCE rejects this shape outright today, so no working deployment can depend on it. Migration note instructs operators to drop the entry and adopt `spec.localSsdMode` + the count label. |
| z3 capacity is regionally scarce                                                               | High       | e2e tests may flake on uncapped runs                                                                     | z3-touching specs gated behind `E2E_Z3_TESTS=true`, mirroring the existing `E2E_GPU_TESTS` pattern.                                                                                     |

---

## Test Plan

### Unit Tests

- `pkg/providers/instance/resolve_localssd_count_test.go` — resolver precedence: bundled value wins; multi-value `In` and non-`In` operators return `CreateError`; cache-miss retryable error.
- `pkg/providers/instance/instance_test.go` — `renderDiskProperties` appends SCRATCH NVMe entries for a configurable count and **none** for a bundled SKU (regression guard for the gcloud-matching body).
- `pkg/providers/instance/localssd_test.go` — `hasBundledLocalSSDs` for API-signal + name-fallback paths, including `-nolssd` siblings.
- `pkg/providers/metadata/localssd_test.go` — `PatchLocalSSDMetadata`: RawBlock and Ephemeral upserts; mode flip removes opposite-mode keys; missing `kube-env` / `kube-labels` → error.
- `pkg/providers/instancetype/types_test.go` — variant emission: bundled SKU produces one pinned variant; configurable family produces a zero-SSD variant plus one per allowed count; ephemeral-storage capacity includes SSD bytes in Ephemeral mode.
- `pkg/providers/instance/instance_test.go` — `orderInstanceTypesByPrice`: equal-price, equal-name variants sort by ascending SSD count (zero-SSD first) so a no-count pod lands on the zero-SSD variant; cross-family price order is preserved.
- `pkg/cache/unavailableofferings_test.go` — monotone suppression: a failure at count `N` marks counts `>= N` unavailable and leaves lower counts schedulable; a count-0 failure suppresses all counts; the minimum-count update is race-free under concurrent marks.

### E2E

Suite at `test/suites/local-ssd/`, three files, 14 specs total (9 positive, 5 failure-mode). Runs in roughly 7 minutes with `--procs=4`, z3 skipped by default (gated on `E2E_Z3_TESTS=true`).

| File                | Specs                                                                                                                                                                                                                                                                     |
|---------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `localssd_test.go`  | 7 entries in a `DescribeTable("Local SSD", ...)`: bundled (c4d Raw/Ephemeral), flex (n2d Raw with pod-set count, n2d zero-SSD-on-empty), family-pin scheduler-filter (c4 picks no-SSD variant, c4a + pod SSD-count=1 picks lssd variant), NodePool-template count source. |
| `mixedpool_test.go` | 2 specs: one pd-balanced pool serves SSD=4 / SSD=2 / no-SSD pods on distinct nodes; one hyperdisk-balanced pool serves c4d + z3 + c4 (gated `E2E_Z3_TESTS`).                                                                                                              |
| `negatives_test.go` | 5 failure-mode specs (`WaitForPodUnschedulable` + `ExpectNoNodeClaim`, no GCE provisioning): bundled count mismatch, count > family max, no-SSD-only family with count label, pod count contradicts NodePool count, NodePool count contradicts bundled count.             |

z3-touching specs are gated by `E2E_Z3_TESTS=true`, matching the existing `E2E_GPU_TESTS` convention.

---

## Acceptance Criteria

The feature is complete when:

- [ ] `spec.localSsdMode` is on `GCENodeClass` with CRD validation and a `v4` hash version.
- [ ] `karpenter.k8s.gcp/instance-local-ssd-count` is a well-known label, emitted as an InstanceType requirement per the variant rules above and as a Node label at provisioning.
- [ ] Nodes provisioned on a bundled-SSD SKU mount the bundled SSDs in the requested mode; nodes provisioned on a configurable family attach the pod-requested count.
- [ ] z3 non-metal ≤ 18 TiB SKUs provision with `MIGRATE`; bare-metal and z3 non-metal > 18 TiB SKUs provision with `TERMINATE`.
- [ ] `disks[].category: local-ssd` is removed from the CRD enum; a NodeClass that sets it is rejected at `kubectl apply`.
- [ ] Unit tests above pass; e2e suite passes on the project's e2e cluster (z3 gate optional).

---

## Migration

Existing deployments cannot have a working `disks[].category: local-ssd` configuration today — GCE rejects every such attempt, so this shape never produced a running node. Removing the enum value is therefore safe in practice:

1. Add `spec.localSsdMode: Ephemeral` (or `RawBlock`) to the NodeClass.
2. Remove any `disks[].category: local-ssd` entries. Once the enum value is dropped, the apiserver rejects a NodeClass that still sets it, so this step is required before upgrading. A stored NodeClass that carries the value keeps working as-is until its next write, but cannot be re-applied until the entry is removed.
3. Set the count via pod `nodeSelector` (`karpenter.k8s.gcp/instance-local-ssd-count: "<N>"`) on configurable families, or via the machine-type instance-type label on bundled SKUs.

No NodeClass-hash invalidation handling is required beyond the existing `GCENodeClassHashVersion` bump to `v4`.

---

## Alternatives Considered

### Count on `GCENodeClass.spec.localSsdCount`

Initial draft put both mode and count on the NodeClass. Rejected because it reproduces the count-proliferation problem (one NodeClass + NodePool per count). It also conflicts with the bundled-SKU case, where the count is fixed by the machine type and any NodeClass value is either redundant or incorrect. The label-driven model lets one NodeClass serve every count the operator wants, including a no-SSD pod scheduling onto the same pool.

### Count on `NodePool.spec.template.spec.requirements` only

A NodePool-side requirement is supported (it's how the resolver reads pool-level pins) but is insufficient as the only mechanism: it forces back to one-NodePool-per-count for pools that want mixed shapes. The label-as-pod-selector path is the primary mechanism; NodePool requirements are a convenience for operators who want every pod in a pool to get the same count.

---

## Future Direction

- **Per-pod `localSsdMode`**: currently the mode is a NodeClass-level setting. A pod-side override (similar to the count label) would let a single pool serve both Ephemeral and RawBlock workloads, which doubles the InstanceType variant count.
- **Honor pod `requests.ephemeral-storage` on RawBlock**: today only Ephemeral mode reports SSD bytes as `ephemeral-storage`. RawBlock workloads have to use the explicit count label. Reporting raw block bytes via a different resource (e.g. an extended resource) would let RawBlock workloads schedule by capacity too.

# Proposal: GKE-style boot-disk swap

- **Status**: Draft
- **Authors**: @dm3ch
- **Created**: 2026-11-11
- **Related Issues**: N/A
- **Related Work**: [PR #731](https://github.com/cloudpilot-ai/karpenter-provider-gcp/pull/731), by @djeebus

---

## Summary

Add boot-disk swap through `GCENodeClass.spec.linuxNodeConfig.swapConfig`, following GKE NodeConfig and Terraform node-pool naming and omitted-value ergonomics. Enabling swap defaults to boot-disk backing, an automatic size, and encryption, and supplies the kubelet settings that make the swap usable by pods. Explicit GiB sizing and opting out of encryption stay available.

The automatic size reproduces what GKE `1.35.8-gke.1796000` resolved for eleven observed machine/disk combinations. It is measured compatibility with that release, not a Google-published formula, and the observations are recorded here so the evidence does not live only in a scratch directory.

Percentage sizing, Local SSD swap profiles and zswap are out of scope.

---

## Motivation

### Problem Statement

GKE node pools with swap enabled create a swap file on the boot disk before the kubelet starts, and GKE defaults the size, backing storage and encryption. Before this proposal the provider cannot request that swap: the kubelet settings let pods use swap the node already has ([#728](https://github.com/cloudpilot-ai/karpenter-provider-gcp/pull/728)), but a Karpenter node has no swap unless a DaemonSet creates one.

[PR #731](https://github.com/cloudpilot-ai/karpenter-provider-gcp/pull/731) added explicit-size boot-disk swap. It spelled the field `linuxNodeConfig.swap`, required a boot-disk profile and a size, and required an explicit kubelet `failSwapOn: false`, so the GKE-shaped minimal configuration

```yaml
linuxNodeConfig:
  swapConfig:
    enabled: true
```

was rejected. The swap file also consumed boot-disk space that neither the scheduler estimate nor the serialized kubelet reservation accounted for, so pods could be scheduled onto space the swap file had already taken.

### Goals

- Follow GKE REST and Terraform node-config naming and nesting, in Kubernetes camelCase.
- Keep swap disabled by default and encryption enabled unless explicitly disabled.
- Resolve the automatic size from evidence, and use the same resolved size for bootstrap metadata, scheduling and the kubelet reservation.
- Keep requested swap independent of the source node pool's swap and zswap settings.
- Preserve explicit host-only swap (`memorySwap.swapBehavior: NoSwap`).
- Refuse configurations that would launch a node without the requested swap.

### Non-Goals

- Ephemeral or dedicated Local SSD swap profiles, zswap, or arbitrary OS setup scripts.
- Percentage-based sizing.
- Changing unrelated kubelet or disk behavior for node classes that do not enable swap.
- Copying every GKE node-pool resource lifecycle behavior.

---

## Proposal

### Overview

The minimal configuration follows GKE:

```yaml
spec:
  linuxNodeConfig:
    swapConfig:
      enabled: true
```

With `enabled: true`, the provider:

1. resolves the swap size for the machine being launched,
2. writes `NODE_SWAP_PROFILE=BOOT_DISK`, the resolved `NODE_SWAP_SIZE` and `NODE_SWAP_ENCRYPTION` into the bootstrap `kube-env`,
3. reserves the swap file from pod ephemeral storage, and
4. defaults the kubelet settings swap needs when the node class omits them.

### API shape

```go
type LinuxNodeConfig struct {
	Hugepages  *HugepagesConfig `json:"hugepages,omitempty"`
	SwapConfig *SwapConfig      `json:"swapConfig,omitempty"`
}

type SwapConfig struct {
	Enabled          bool                  `json:"enabled,omitempty"`
	EncryptionConfig *SwapEncryptionConfig `json:"encryptionConfig,omitempty"`
	BootDiskProfile  *SwapBootDiskProfile  `json:"bootDiskProfile,omitempty"`
}

type SwapBootDiskProfile struct {
	SwapSizeGib *int64 `json:"swapSizeGib,omitempty"` // minimum 1
}
```

- An omitted `swapConfig`, an omitted `enabled`, and `enabled: false` all create no provider-managed swap. In every case the provider removes all `NODE_SWAP_*` and `NODE_ZSWAP_*` entries before writing its own, so a node never inherits the source pool's swap or zswap configuration.
- Omitted `bootDiskProfile` and an omitted `swapSizeGib` select the automatic size. An explicit size is never clamped: a request larger than half of the boot disk is rejected.
- `encryptionConfig.disabled: true` turns off encryption. The file is otherwise encrypted with a key the node generates at boot (`/dev/mapper/encswap`).
- Admission rejects `enabled: true` with `kubeletConfiguration.failSwapOn: true`, and rejects `bootDiskProfile`/`encryptionConfig` when swap is not enabled.
- With `enabled: true` the provider defaults omitted `kubeletConfiguration.failSwapOn` to `false` and omitted or empty `memorySwap.swapBehavior` to `LimitedSwap`, replacing source-pool values. An explicit `NoSwap` survives.

### Automatic sizing, as measured

The GKE node bootstrap consumes a pre-resolved size: `setup-swap` in `configure-helper.sh` reads `NODE_SWAP_SIZE` and calls `fallocate` with it. The size therefore comes from the GKE control plane, and the provider has to resolve the same value itself.

Eleven separately approved zero-node observations on GKE `1.35.8-gke.1796000` (generated configuration, no worker instances) fit this integer calculation:

```text
ramGiB  = rawMachineMemoryMb / 1024      # integer truncation first
swapGiB = min(ramGiB + min(ramGiB, 8), bootDiskGiB / 2)
```

| Image                       | Machine memory | Boot disk | Resolved swap |
|-----------------------------|---------------:|----------:|--------------:|
| COS                         |          2 GiB |   100 GiB |         4 GiB |
| COS                         |          4 GiB |   100 GiB |         8 GiB |
| COS                         |          8 GiB |   100 GiB |        16 GiB |
| COS                         |         16 GiB |   100 GiB |        24 GiB |
| COS                         |         32 GiB |   200 GiB |        40 GiB |
| COS                         |         64 GiB |   400 GiB |        72 GiB |
| COS                         |       7.75 GiB |   100 GiB |        14 GiB |
| COS                         |       8.25 GiB |   100 GiB |        16 GiB |
| COS                         |          8 GiB |    20 GiB |        10 GiB |
| COS                         |         16 GiB |    21 GiB |        10 GiB |
| Ubuntu, encryption disabled |         16 GiB |   100 GiB |        24 GiB |

The 7.75 GiB case shows the truncation happens before doubling: the result is 14 GiB, not `floor(2 × 7.75) = 15 GiB`. The 21 GiB disk caps at 10 GiB, not 10.5 GiB. Every generated kubelet config in these observations had `failSwapOn: false` and `memorySwap.swapBehavior: LimitedSwap`, which is what the provider now defaults to.

The memory basis is the machine's raw Compute memory, not the schedulable memory that subtracts VM overhead. The expectations above are fixed unit-test inputs and are never produced by the resolver itself, so a change in the algorithm fails the tests instead of silently agreeing with itself.

This is compatibility with one tested release. Other GKE versions, extreme memory shapes and running-node outcomes are untested, and GKE may change its defaults; the resolver is a single function to update when that happens.

### Effective boot disk

The provider's ephemeral-storage estimate falls back to 100 GiB when no boot disk is configured, while the renderer previously only attached `spec.disks`. Merely relying on the estimate would therefore admit a node whose boot disk never exists.

A swap-enabled node class that configures no boot disk now gets a provider-owned 100 GiB boot disk, matching the documented GKE default and the existing estimate. The disk type follows the machine family's known compatibility data: `pd-standard` where supported, then `pd-balanced`, then `hyperdisk-balanced`; a family with no compatibility data fails rather than guessing. Machine families differ, so this is provider policy rather than a claim that GKE picks one disk type everywhere. A configured boot disk is used unchanged, but it must set `sizeGiB`, because the half-disk limit cannot be otherwise checked. Several boot disks fail the launch.

Nodes that do not enable swap keep exactly the previous disk behavior.

### Storage accounting

The swap file occupies the boot disk before the kubelet counts ephemeral storage. The provider therefore:

1. recomputes the ephemeral-storage kube-reserved baseline against `bootDiskGiB − swapGiB`, using the existing reservation helper,
2. applies an explicit `kubeletConfiguration.kubeReserved` override, and
3. adds the swap size exactly once.

The eviction threshold stays on the whole disk, and CPU, memory and all non-swap reservations are unchanged. Under the current helper, a 30 GiB disk with 15 GiB of swap reserves 8 GiB baseline + 15 GiB swap = 23 GiB, keeps the 3 GiB eviction threshold, and models 4 GiB usable; a 50 GiB disk with 24 GiB of swap reserves 13 + 24 = 37 GiB, keeps 5 GiB eviction, and models 8 GiB usable. An explicit 20 GiB override with a 4 GiB swap file reserves 24 GiB. These are provider accounting values, not measured physical headroom or GKE runtime parity.

The same reservation must reach the serialized kubelet configuration: the node-class overlay previously replaced the computed value with the user's 20 GiB, and the fix restores the effective value after the overlay instead of reapplying the rest of the reservation pipeline. Pod storage on ephemeral Local SSDs is not charged for the swap file; raw-block Local SSDs leave pod storage on the boot disk. Node capacity stays the size of the disk.

### Unsupported releases and images

Enabled swap requires a bootstrap that creates the file. The provider reads the release from the source template's versioned `KUBE_MANIFESTS_TAR_URL`, not from the control-plane version: the node downloads that manifests archive, copies its `gci-configure-helper.sh` in as `/home/kubernetes/bin/configure-helper.sh`, and runs it, so the manifests archive is the bootstrap code that executes. `SERVER_BINARY_TAR_URL` names the Kubernetes server binaries instead and can be a different release, so it does not decide support. GKE build numbers are compared numerically, because string ordering puts `gke.999` after `gke.1341000`. Releases older than `1.34.1-gke.1341000`, unknown releases, custom image families and missing machine facts fail the launch with an actionable error instead of creating a node without swap. This is a known-incompatibility guard, not a capability-discovery subsystem, and it does not prove every pinned COS or Ubuntu image activates swap at runtime.

---

## Risks and Mitigations

| Risk                                           | Likelihood | Impact                                          | Mitigation                                                                                      |
|------------------------------------------------|------------|-------------------------------------------------|-------------------------------------------------------------------------------------------------|
| GKE changes its automatic sizing               | Medium     | Nodes get a different swap size than a GKE pool | Sizing lives in one measured function; the proposal records the tested release and observations |
| Half-disk limit checked against the wrong disk | Medium     | Node launch rejected by GKE, or oversized swap  | One effective-disk decision shared by scheduling, rendering and bootstrap                       |
| Scheduler and kubelet disagree on storage      | Medium     | Pods evicted or unschedulable                   | One computed reservation per configuration, retained after the node-class overlay               |
| Source pool swap leaks into the node           | Low        | Local SSDs reserved unexpectedly                | All `NODE_SWAP_*`/`NODE_ZSWAP_*` entries are removed first                                      |

---

## Test Plan

### Unit Tests

- Every one of the eleven observed sizing cases, with fixed expectations.
- Explicit size, half-disk boundary, oversize rejection, automatic size below 1 GiB of memory.
- Default and explicit boot disks, machine-family disk type selection, conflicting and unsized boot disks.
- Kubelet admission rules evaluated as CEL, not compared as strings: minimal enablement, `failSwapOn` conflict, disabled settings, half-disk limit, disks without a `boot` key.
- Storage accounting including the two fixed disk/swap examples, explicit overrides, Local SSD modes and positive usable storage.
- Serialized kubelet reservation after the node-class overlay, kubelet swap defaults, explicit `NoSwap`.
- Bootstrap manifests release floor with numeric build comparison, mismatched manifests/server releases, unknown releases and custom image families.
- Cache identity across automatic, explicit and disabled configurations, and source metadata immutability.

### E2E / Integration Tests

Four cases in the existing `kubelet_config` suite: COS and Ubuntu, each encrypted and unencrypted. One COS case omits `spec.disks` and `spec.kubeletConfiguration`, so it exercises the provider-created 100 GiB boot disk, the automatic 24 GiB swap size and the kubelet defaults. One Ubuntu case sets an explicit 4 GiB size with an explicit 20 GiB `kubeReserved` and asserts the final 24 GiB reservation. Every case reads the running kubelet's configuration, then runs a short unprivileged, read-only host probe that checks the active swap backing in `/proc/swaps`: the dm-crypt device whose `/sys/block/<dev>/dm/name` is `encswap` when encryption is on, and GKE's plain swap file with no `encswap` device when it is off. The cases also check that the node's allocatable ephemeral storage leaves room for the reservation and eviction threshold the kubelet reports, using the node's own filesystem capacity. They are compiled and dry-run locally; running them against GKE needs separate approval.

---

## Acceptance Criteria

The feature is complete when:

- [X] The API and defaults match the tested GKE contract and all measured sizing cases.
- [X] One effective boot disk and size drive scheduling, rendering, bootstrap metadata and the final kubelet reservation.
- [X] Known-incompatible configurations fail explicitly, and nodes without swap are unchanged.
- [X] Local unit, admission, generation and documentation checks pass.
- [ ] The four E2E cases run against a GKE cluster (separate approval required).

---

## Migration

No released API changes. `swapConfig` is a new field, and #731's unmerged `linuxNodeConfig.swap` spelling never shipped. Operators adding swap to a node class that configures no disks get a provider-created 100 GiB boot disk; set `spec.disks` explicitly to choose a different boot disk. See [Swap](docs/examples/advanced.md#swap).

---

## Alternatives Considered

### Require an explicit swap size

Rejected: it diverges from GKE's minimal `enabled: true` configuration, which operators already know from node pools, and was explicitly not the approved contract.

### Delegate sizing to the node bootstrap

Rejected: the GKE bootstrap only consumes a resolved `NODE_SWAP_SIZE`, and a node that resolves its own size cannot be accounted for before launch.

### Skip automatic sizing and use a provider-specific policy

Rejected: the approved contract keeps GKE-compatible defaults rather than inventing a different policy.

### Percentage sizing

Out of scope. It adds rounding questions to a contract that is currently measured in whole GiB.

---

## Future Direction

Ephemeral and dedicated Local SSD swap profiles, percentage sizing and zswap could follow the same resolver, once each has its own evidence and accounting model.

---

## Open Questions

1. **Automatic sizing outside the measured range**: the resolver's shape is the best fit to eleven observations; other GKE releases may differ. Re-verify on the cluster version the provider targets before claiming parity.
2. **Runtime activation**: zero-node observations prove the generated configuration, not that a running node activates swap. The E2E cases close that gap when they are run.

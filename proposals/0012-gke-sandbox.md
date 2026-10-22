# Proposal: GKE Sandbox (gVisor)

- **Status**: Accepted; implemented, live acceptance pending
- **Authors**: CloudPilot AI contributors
- **Created**: 2026-10-22
- **Related Issues**: [#504](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/504)

## Summary

Add optional `GCENodeClass.spec.sandboxConfig.type: GVISOR`, matching GKE's sandbox configuration. Omission continues to mean ordinary capacity. Sandboxing uses public GKE COS images, immutable public installer artifacts, and an explicitly maintained bootstrap compatibility policy. It does not require a native sandbox pool.

The implementation includes API/admission, image/artifact resolution, runtime metadata, hardware compatibility, NodePool reconciliation, scheduling/launch guards and durable lifecycle identity/drift. Operator guidance is in [Sandbox nodes](../docs/sandbox-nodes.md), with a [complete minimal example](../docs/examples/sandbox.md). Offline regression and zero-node native-template evidence do not prove reconstructed-node boot, isolation or CUDA execution; live acceptance remains separate.

## Motivation

Ordinary COS bootstrap does not enable gVisor. A sandbox workload must not receive ordinary capacity, and ordinary workloads without the appropriate toleration must not trigger sandbox capacity. Copying runtime settings from a source pool also leaks its hardware policy and mutable installer selection into new nodes.

Goals are native scheduling semantics, fail-closed bootstrap compatibility, immutable runtime identity, and support for the provider's compatible CPU and built-in GPU catalog. MicroVM support is explicitly excluded and tracked by [#707](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/707).

## Proposal

### API and images

```yaml
spec:
  sandboxConfig:
    type: GVISOR
  imageSelectorTerms:
    - family: ContainerOptimizedOS
      channel: cluster
```

The singleton object requires `type`, whose only accepted value is `GVISOR`. There is no enable boolean, installer-version input, or sandboxType alias. Admission examines every selector, rejecting Ubuntu aliases/families, mixed terms, contradictory `imageFamily`, and GPU time-sharing. Explicit image IDs must resolve to actual READY, non-deprecated, supported public GKE COS images in `gke-node-images`; an override or custom/copied image is not provenance.

CPU sandbox nodes use ordinary COS for amd64 or Arm64. GPU nodes use real public exact-GKE-build `-gv-nvda` or older `-gvisor-nvda` catalog variants, including Arm64 where published. CPU and GPU COS builds can differ. Discovery must honor explicit IDs, channel builds and explicit COS pins, never fabricate GPU names, change a pinned build, or fall back to ordinary GPU images. Missing compatible GPU variants mean no GPU candidates, not permission to use CPU images.

### Public immutable installer

Strictly recognize the selected image's `gvisor-version` label, for example `20260323-0_rc03` becomes `20260323.0_RC03`. Fetch the public OCI manifest/index from `https://gke.gcr.io/v2/gke-gvisor-installer/manifests/<tag>`. Validate content SHA256 against the registry digest header, schema/media types and Linux platform descriptors (or a single manifest's digest-verified config). Resolve to `gke.gcr.io/gke-gvisor-installer@sha256:...`. Each resolved image carries its own installer version and digest in status; the status reconciler must copy this identity rather than resolving one artifact for the whole NodeClass.

Use bounded time/body sizes, same-origin redirects, a fixed public origin and bounded positive caches with five-minute refresh. Missing labels, unknown formats, unavailable artifacts and unsupported platforms fail explicitly. There is no `latest`, guessed fallback version, or release-note scraping. Ordinary and sandbox image cache identities are separate.

### Bootstrap consumer compatibility

The effective source `KUBE_MANIFESTS_TAR_URL` and `SERVER_BINARY_TAR_URL` must name the same versioned GKE release under `https://storage.googleapis.com/gke-release/kubernetes/release/` or the accepted `gke-release-asia`/`gke-release-eu` bucket mirrors. Every entry in a native mirror list is validated. The master version is not a substitute for this effective kubelet/bootstrap version.

Download manifests only from this validated public path; do not execute scripts or extract to disk. Bound compressed data to 2 MiB, total decompression to 16 MiB, and each member to 2 MiB. Compare complete SHA256 fingerprints at these exact archive paths, rejecting missing, duplicate, non-regular or changed consumers:

- `kubernetes/gci-trusty/gci-configure-helper.sh`
- `kubernetes/gci-trusty/gke-internal-configure-helper.sh`
- `kubernetes/gci-trusty/installable.py`

The initial accepted profile comes from public GKE `v1.35.6-gke.1250001`. A reduced public-consumer fixture, the complete original public archive and a provenance/fingerprint record accompany the resolver. Other releases are acceptable only when all consumers match a reviewed profile, or after reviewing new consumers/native policy and regression/live evidence. `SandboxBootstrapUnsupported` blocks launch for unknown schemas; it is not an insufficient-capacity signal. Positive cache identity includes release, schema and policy revision, and refreshes within five minutes.

A match proves consumer compatibility, **not discovery of complete GKE control-plane hardware/security policy**. The durable VM profile identity must combine accepted schema, policy revision and validated hardware-policy variant. Do not infer historical identity from today's NodeClass. Unknown consumers may block new sandbox provisioning after an ordinary GKE upgrade until a controller update; this strict maintenance/upgrade tradeoff is explicitly accepted.

### Runtime and lifecycle implementation

The instance provider retains existing cluster-specific bootstrap handling; sandbox discovery/compatibility does not reconstruct, log or separately persist source credentials. Apply a focused typed overlay, merge only `gvisor.gvisor` in `RENDERED_INSTALLABLES`, preserve unrelated installables and reject malformed JSON. Both enable and disable remove inherited sandbox-enabling settings before applying desired state. Synchronize sandbox labels/taints in all native metadata representations.

The reviewed native policy is `xemu`, SMT enabled and nftables reproduction. Core tags are enabled for Intel/mixed-vendor amd64 families, omitted for AMD-only families and Arm. Classify documented machine families (including custom machines), not just architecture or source-pool hardware. Keep full vCPU capacity consistent with SMT. Independently validate GPU model, selected image release, effective bootstrap release and default/latest driver mode: H100/A100/L4/T4 patch floors, later H200/B200/GB200/RTX PRO 6000 floors, and no V100/P100, unsupported fractional models or time-sharing. Do not filter historical instances out of discovery.

The `nodepool.sandbox` reconciler adds only the template label `sandbox.gke.io/runtime=gvisor`, its permanent `NoSchedule` taint and the top-level `karpenter.k8s.gcp/sandbox-ownership` annotation, with independent label/taint ownership and optimistic patches. Preexisting identical user fields, conflicts and unrelated fields are preserved. Disable/reference changes remove only still-owned canonical values. Gate scheduling on persisted consistent fields and guard stale NodeClaims in Create. Advertise runtime `In: gvisor` for sandbox and `DoesNotExist` for ordinary capacity. Separate affected static-instance-type caches as well as image caches. Configuration/artifact failures must not poison capacity caches.

RuntimeClasses remain GKE/administrator-owned, including custom names targeting handler `gvisor`. No RuntimeClass RBAC or controller mutation is added. Admission supplies selectors/tolerations/overhead; core Karpenter already accounts for that overhead. Explicitly tolerated ordinary Pods may use sandbox capacity.

VMs are stamped with runtime, immutable installer and complete bootstrap schema/policy/hardware identity using reserved provider keys, bound to the launch NodeClass UID/hash and genuine NodePool owner UID when available. List/Get/adoption and NodeClaim conversion use VM facts, including actual boot-disk source images, rather than today's NodeClass/cache or mutable claim image. Ordinary/stale-runtime adoption is rejected. NodeClass/image drift and installer/profile drift preserve historical VM discovery/GC even when no longer launchable. Unknown actual image/profile facts produce comparison errors rather than invented drift.

## Migration

Omitted sandbox configuration preserves ordinary behavior. Upgrade controller/CRDs together before opting in. Existing minimal controller custom roles need the declarative `compute.disks.get` permission in `deploy/iam/karpenter-controller-role.yaml` for factual adoption/drift; `deploy/terraform/` reads this role file. No RuntimeClass ownership/RBAC is added. Enabling requires safe GKE/admin-owned RuntimeClass scheduling, compatible public COS artifacts and outbound access to `gke.gcr.io` and public GKE artifact mirrors at `storage.googleapis.com`. Keep the ordinary system pool; no native sandbox pool is required.

New GitOps pools can explicitly declare both canonical runtime fields before reconciliation; these remain user-owned and must both be removed when disabling or changing to an ordinary class. Do not forge ownership records or ignore all labels/taints. Existing controller-added fields retain their recorded ownership. Scheduling gates on persisted fields instead of mutating its input. See [GitOps ownership](../docs/sandbox-nodes.md#gitops-ownership).

GKE upgrades with changed consumers can block new sandbox provisioning until a reviewed controller compatibility update; there is no opt-out or semver-only bypass. Image pins do not bypass profile/artifact compatibility. Disable changes desired identity and cleans inherited enabling settings on new ordinary nodes, not running VMs; migrate workloads and plan disruption-controlled replacement before disabling/downgrading. See [Migration guidance](../MIGRATION.md#opt-in-gke-sandbox-and-boot-disk-read-permission).

## Alternatives Considered

- Copy a native sandbox pool: introduces a product prerequisite, source hardware leakage and credential-handling risk.
- Synthesize GPU names or guess installer versions: violates public artifact identity and cannot honor exact build/COS pins.
- Semver/marker-only compatibility: fails to detect changed consumer schemas.
- Own RuntimeClasses or add scheduler special cases: duplicates admission/core responsibilities and breaks custom class names.

## Test Plan and Acceptance Criteria

Offline tests cover admission/hash behavior, strict public image selection and pinning, both architectures/roles, immutable OCI identities, bounded HTTP and archive handling, cache separation/refresh, fixture-backed compatibility, metadata preservation/cleanup, hardware/version matrices, NodePool ownership/gates, stale-claim/adoption/drift and ordinary regressions. The reviewed public bootstrap fixture is GKE `1.35.6-gke.1250001`; matching consumers on another release is compatibility evidence, not all-release native policy or runtime proof.

Before feature acceptance, separately authorized reconstructed CPU amd64/Arm64 and supported-GPU live tests must prove boot, runtime handling/isolation, networking/storage/system DaemonSets, CUDA and disable/drift behavior. Zero-node native comparisons prove template/artifact parity only, not runtime execution. Live acceptance/deployment requires separate environment-specific authorization; implementation and generation do not authorize cloud/Kubernetes/IAM writes.

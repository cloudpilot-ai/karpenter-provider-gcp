# Proposal: Eliminate Karpenter-Managed Node Pool Dependency

- **Status**: Provisional
- **Authors**: @dm3ch
- **Created**: 2026-04-17
- **Related Issues**: [#255](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/255)

---

## Summary

Karpenter currently creates up to four GKE node pools solely to read back the bootstrap metadata (kube-env) GKE embeds in their instance templates. This breaks in clusters with org policies such as `constraints/compute.requireShieldedVm` or `constraints/gcp.restrictNonCmekServices` because Karpenter cannot create compliant pools.

This proposal replaces pool creation with discovery: Karpenter selects an existing RUNNING cluster pool as the bootstrap source, and patches kube-env for OS type and architecture at provisioning time. A fallback pool is created only as a last resort when no RUNNING pool is available.

---

## Motivation

### Problem Statement

The current architecture creates node pools as a workaround to obtain data that GKE embeds in pool instance templates:

- **Cluster-level constants** (CA certificate, master endpoint, IP ranges, feature flags) — identical across all pools
- **Architecture-specific binary metadata** (`SERVER_BINARY_TAR_URL`/`SERVER_BINARY_TAR_HASH`) — derivable from GKE version string plus a 65-byte public GCS file
- **OS-specific kube-env patches** (os-distribution label, BFQ scheduler flags) — simple regex replacements
- **Per-pool identity credentials** (`TPM_BOOTSTRAP_CERT`, `KUBE_PROXY_TOKEN`) — the only fields that genuinely require reading a pool

Creating pools to access the first three categories is unnecessary. The fourth category can be satisfied by reading from any already-existing cluster pool rather than one created by Karpenter.

### Impact

- Clusters under strict org policies cannot use Karpenter today without manual pre-creation of pools or stop-gap env vars.
- Every new GKE cluster requires Karpenter to create 2–4 pools before provisioning any workload nodes. This adds latency, consumes quota, and creates IAM-visible resources that operators did not ask for.
- Four separately-managed pools increase surface area for operational failures (wrong machine type unavailable in region, pool stuck in PROVISIONING, etc.).

### Goals

- Karpenter provisions nodes without creating GKE node pools under normal operating conditions.
- All existing node variants (COS amd64, Ubuntu amd64, COS arm64, Ubuntu arm64, spot, on-demand) continue to work.
- Clusters under `compute.requireShieldedVm` and `gcp.restrictNonCmekServices` work without additional configuration.
- Operator can optionally pin the pool used as a bootstrap source.
- Graceful handling when all pools are temporarily unavailable (e.g., cluster upgrade in progress).

### Non-Goals

- Full kube-env reconstruction from GKE APIs alone with no pool reads (future direction, not this proposal).
- Changes to the `GCENodeClass` or `NodePool` API surface.
- Support for Windows nodes (not currently supported regardless of this change).
- GKE Autopilot clusters (node pool API surface differs; explicit non-goal for this proposal).

---

## Proposal

### Overview

The core change is in `pkg/providers/nodepooltemplate/`: replace the four hardcoded pool creation calls with a pool discovery and selection algorithm. All downstream code (instance building, metadata patching) is unchanged except for two new patch functions.

```
Before:
  Karpenter creates pools → reads back templates → provisions nodes

After:
  Karpenter discovers existing pools → selects best → provisions nodes
```

### Bootstrap Data Sources

The table below maps each kube-env field group to its source after this change. "Any pool" means whichever RUNNING pool the selection algorithm picks — arch of the source pool does not matter.

| Group | Fields | Source |
|---|---|---|
| 1 — Cluster constants | CA cert, master endpoint, IP ranges, feature flags, … | Any pool's template |
| 2 — Arch-specific binary | `SERVER_BINARY_TAR_URL`, `SERVER_BINARY_TAR_HASH` | Source arch detected from pool's URL; patched to target arch; hash from GCS `.sha512` sidecar |
| 3 — OS-specific | `gke-os-distribution`, BFQ scheduler flags | Patched at provisioning time (`PatchKubeEnvForOSType`) |
| 4 — Per-pool credentials | `TPM_BOOTSTRAP_CERT`, `KUBE_PROXY_TOKEN`, NPD config | Any pool's template |
| 5 — Node-specific | arch label, machine-family, provisioning model, max-pods | Patched at provisioning time (existing functions, no change) |
| 6 — Boot images | Source image URL per OS + arch | COS: pool template boot disk; Ubuntu: `ubuntu-os-gke-cloud` image catalog |

### Design Details

#### Pool Selection Algorithm

On startup and on each template refresh cycle, Karpenter selects a bootstrap source pool using the following priority order:

```
1. If DEFAULT_NODEPOOL_TEMPLATE_NAME is set → use that pool; error if not RUNNING.
2. If default-pool exists and is RUNNING → use it.
3. Sort remaining pools by name; use the first RUNNING pool.
4. If no RUNNING pool found → retry with backoff (transient during cluster upgrades).
   If retry limit exceeded → create karpenter-default as last-resort fallback (see below).
```

The selected pool name is stored in the provider struct, refreshed every sync cycle. `GetInstanceTemplates()` returns a single template keyed by the selected pool name. The selected pool name is logged at INFO on every refresh cycle so operators can observe which pool is in use, including across restarts.

No arch-based or OS-based filtering is needed. `PatchKubeEnvForArch` and `PatchKubeEnvForOSType` handle all differences at provisioning time regardless of the source pool's architecture or image type.

#### Last-Resort Fallback Pool

In the unlikely event that no RUNNING pool is available after retries (e.g., unusual cluster state), Karpenter creates a single `karpenter-default` pool. The implementation will make a best effort to be compatible with all known optional GCE org policies:

- Minimal `NodeConfig`: only `imageType` (COS_CONTAINERD) and `serviceAccount`; no explicit machine type, no custom labels or taints
- Shielded VM config enabled by default (`enableSecureBoot`, `enableIntegrityMonitoring`) to satisfy `compute.requireShieldedVm`
- CMEK-related options left unset to avoid triggering `gcp.restrictNonCmekServices`
- 0 initial nodes — pool is never used to run workloads

If the fallback creation still fails due to an org policy that cannot be automatically satisfied, Karpenter logs a clear error and halts provisioning. The operator can then create any RUNNING pool manually and set `DEFAULT_NODEPOOL_TEMPLATE_NAME` to point Karpenter at it.

`karpenter-default` is not preferred over existing cluster pools. It is only used when the fallback creation path fires — at which point it is the only candidate.

#### Architecture-Specific Binary Metadata (Group 2)

GCS publishes `.sha512` sidecar files alongside every GKE release tarball:

```
https://storage.googleapis.com/gke-release/kubernetes/release/{VERSION}/
  kubernetes-server-linux-{ARCH}.tar.gz        ← tarball
  kubernetes-server-linux-{ARCH}.tar.gz.sha512 ← 128-char raw hex SHA-512
```

Confirmed on all three mirrors (`gke-release`, `gke-release-eu`, `gke-release-asia`). `SERVER_BINARY_TAR_HASH` in kube-env is raw 128-char SHA-512 hex with no prefix (verified against GKE 1.35.1 live kube-env).

`PatchKubeEnvForArch` (new function in `pkg/providers/metadata/utils.go`) patches `SERVER_BINARY_TAR_URL` and `SERVER_BINARY_TAR_HASH` to match the target node's architecture, regardless of the source pool's architecture:

1. Detects source arch from `SERVER_BINARY_TAR_URL` content (`linux-amd64` or `linux-arm64`)
2. If source arch == target arch → no-op
3. Otherwise substitutes `linux-{source}` → `linux-{target}` in each mirror URL
4. Extracts GKE version from the URL path
5. Fetches `{primary_url}.sha512` (128-char raw hex) using an HTTP client; caches by `{target-arch}:{version}` in a `sync.Map`
6. Replaces both `SERVER_BINARY_TAR_URL` and `SERVER_BINARY_TAR_HASH` in kube-env

This follows a "query a known public endpoint by version and arch" pattern, avoiding the need to create a template resource. Note: the GCS URL scheme (`gke-release` bucket path layout) has no documented stability contract. If Google changes the path format, `PatchKubeEnvForArch` will require an update. Confirmed on all three mirrors (`gke-release`, `gke-release-eu`, `gke-release-asia`) as of GKE 1.35.

#### OS-Type Patching (Group 3)

Add `PatchKubeEnvForOSType(metadata *compute.Metadata, imageFamily v1alpha1.ImageFamily)` to `pkg/providers/metadata/utils.go`:

| Field | COS pool value | Action for Ubuntu |
|---|---|---|
| `gke-os-distribution=cos` in `AUTOSCALER_ENV_VARS` | `cos` | regex replace → `ubuntu` |
| `gke-os-distribution=cos` in `KUBELET_ARGS` | `cos` | regex replace → `ubuntu` |
| `ENABLE_NODE_BFQ_IO_SCHEDULER` | `"true"` | remove line |
| `NODE_BFQ_IO_SCHEDULER_IO_WEIGHT` | `"1200"` | remove line |

This follows the same pattern as the existing `PatchKubeEnvForInstanceType` and eliminates the Ubuntu-specific template pools.

#### Group 4 Credential Reuse

Verified against GKE 1.35.1 (COS `default-pool` vs Ubuntu `ubuntu-pool` on the same cluster):

- **`KUBE_PROXY_TOKEN`** — **identical across pools**. Cluster-scoped; cross-pool reuse is safe with no patching needed.
- **`TPM_BOOTSTRAP_CERT` / `TPM_BOOTSTRAP_KEY`** — **different per pool** (unique x509 cert and RSA key pair per pool). Verified on GKE 1.35.1: the cert Subject encodes the pool name (`O=gke:nodepool:name:{pool}`, `CN=kubelet-nodepool-bootstrap`), and each pool has its own RSA key pair. The Issuer is the cluster CA UUID. Since Karpenter today uses `karpenter-default`'s cert/key for both COS and Ubuntu nodes without bootstrap failures (different OS, same pool cert), GKE likely validates CA signature only — not pool membership in the Subject. Cross-pool reuse (cert Subject names a different pool than the provisioned node belongs to) is expected to work on the same basis. Must be validated with an explicit cross-pool PoC in Phase 0.
- **`NODE_PROBLEM_DETECTOR_ADC_CONFIG`** — **different per pool**. The audience URL and credential_source URL both embed the pool name:
  ```
  https://.../nodePools/{pool-name}/systemComponents/node-problem-detector/generateNodeServiceAccountToken
  ```
  `PatchNodeProblemDetectorConfig` (new function) must replace the embedded pool name with the correct source pool name for provisioned nodes. The patch is required, not optional.

#### Code Changes

| File | Change |
|---|---|
| `pkg/providers/nodepooltemplate/nodepooltemplate.go` | `Create()` → no-op unless no RUNNING pool found (fallback only). Add `discoverSourcePool()` with priority-based selection. |
| `pkg/providers/instance/instance.go` | `resolveNodePoolName()` → `resolveSourcePoolName()`. Single source pool for all OS families and both arches. |
| `pkg/providers/metadata/utils.go` | Add `PatchKubeEnvForOSType`, `PatchKubeEnvForArch`, `PatchNodeProblemDetectorConfig`. |
| `pkg/operator/options/options.go` | Add `DEFAULT_NODEPOOL_TEMPLATE_NAME` env var (optional, default empty). |

Existing functions — `getInstanceTemplate`, `resolveInstanceGroupZoneAndManagerName`, `resolveInstanceTemplateName`, `PatchKubeEnvForInstanceType`, `SetProvisioningModel`, `SetMaxPodsPerNode`, `RemoveGKEBuiltinLabels` — are reused unchanged.

---

## Risks and Mitigations

### Selected pool is deleted

**Scenario**: Operator deletes the pool Karpenter selected as bootstrap source.

**Impact on running nodes**: None. The instance template is read at VM creation time only. Nodes that have already bootstrapped and registered continue running independently.

**Impact on new provisioning**: The next `GetInstanceTemplates()` call finds no pool. Provisioning halts until resolved.

**Mitigation**:
- The refresh cycle re-runs selection. If another RUNNING pool exists, it is promoted automatically with no operator action.
- If no pool remains, the fallback creates `karpenter-default` (COS, 0 nodes) — distinct from the current 4-pool creation path.
- `DEFAULT_NODEPOOL_TEMPLATE_NAME` produces a clear error message naming the expected pool if it is missing.

### GKE control-plane or node-pool upgrade

**Scenario**: GKE upgrades the control plane (changes `currentMasterVersion`) or upgrades a node pool (rotates its instance template).

**Impact on running nodes**: None. Already-bootstrapped nodes are not affected by template rotation.

**Impact on new provisioning**: The `NodePool → InstanceGroupManager → current InstanceTemplate` chain automatically resolves to the new template. Within one refresh TTL (currently 5 minutes), new nodes pick up the post-upgrade template.

**Mitigation**: No specific action needed. The existing traversal chain handles this correctly. The brief window where cached old-template metadata is used causes no known failures — old and new templates carry the same cluster version fields.

### Template updated between cache fill and node creation

**Scenario**: GKE rotates the template between when Karpenter's cache was populated and when a node is actually created.

**Impact**: The node gets the previous template's metadata. Since GKE upgrades node pools after control-plane upgrades, old and new pool templates are compatible with the same cluster version. No known failure mode.

**Mitigation**: Acceptable within the cache TTL window. No change needed.

### Multiple candidate pools — non-deterministic selection

**Scenario**: Cluster has several RUNNING pools, each potentially with different custom metadata or image settings.

**Impact**: Group 1 (cluster constants) and Group 4 (credentials) are identical across pools — verified across four pools on a live GKE 1.35.1 cluster. Custom `Properties.Metadata` keys set by operators are the only divergence risk.

**Mitigation**: Selection is deterministic — `default-pool` is preferred, then alphabetical by name. The selected pool name is logged at INFO on every refresh cycle, so operators can observe the current source pool and detect changes. Operators who need a specific pool can pin it via `DEFAULT_NODEPOOL_TEMPLATE_NAME`.

### `NODE_PROBLEM_DETECTOR_ADC_CONFIG` pool name validation

**Scenario**: GKE validates that the audience URL's embedded pool name corresponds to a pool the node is a member of. Karpenter nodes using another pool's NPD config fail authentication.

**Finding**: Confirmed that `NODE_PROBLEM_DETECTOR_ADC_CONFIG` embeds the source pool name in both the audience URL and credential_source URL (verified against GKE 1.35.1). The patch is therefore required, not conditional.

**Mitigation**: `PatchNodeProblemDetectorConfig` replaces the embedded pool name with the name of the actual source pool Karpenter selected. Validate in e2e that NPD pods reach Running state on Karpenter-provisioned nodes.

### `KUBE_PROXY_TOKEN` pool-scoped RBAC

**Scenario**: GKE issues the token against a pool-scoped RBAC subject. Using another pool's token causes kube-proxy auth failures.

**Likelihood**: Low — no reported failures in current Karpenter operation where one pool's token is used for a different OS family.

**Mitigation**: Validate in e2e by inspecting kube-proxy pod logs on Karpenter-provisioned nodes using a non-owning pool's credentials.

### No RUNNING pool found

**Scenario**: All cluster pools are in PROVISIONING state during a GKE cluster upgrade (transient), or an unusual cluster state where all pools are unavailable.

**Mitigation**: Retry with backoff — the normal case (upgrade in progress) resolves within minutes. If retries are exhausted, create `karpenter-default` as a last-resort fallback with minimal, policy-safe config (see Pool Selection Algorithm above). Log pool states clearly at each retry so operators can diagnose the situation.

---

## Test Plan

### Unit Tests

- `discoverSourcePool` selection: mock pool lists with various combinations (no pools, `default-pool` present, no `default-pool` → first by name, `DEFAULT_NODEPOOL_TEMPLATE_NAME` set, non-RUNNING pools skipped)
- `PatchKubeEnvForOSType`: COS→Ubuntu OS distribution replacement, BFQ field removal, no-op on COS input
- `PatchKubeEnvForArch`: URL substitution, hash replacement from mock HTTP server returning `.sha256` content, cache hit avoids second HTTP call
- `PatchNodeProblemDetectorConfig`: pool name replacement in audience URL, no-op when field absent

### E2E Test Matrix

| Variant | Bootstrap source | Image source | Patches |
|---|---|---|---|
| COS amd64 on-demand | discovered pool | pool template boot disk | arch, machine-family, provisioning |
| COS amd64 spot | same | same | + gke-provisioning=spot |
| Ubuntu amd64 on-demand | same | `ubuntu-os-gke-cloud` lookup | + gke-os-distribution, -BFQ |
| Ubuntu amd64 spot | same | same | + gke-provisioning=spot |
| COS arm64 on-demand | same | `gke-node-images` arm64 | arch=arm64, URL/hash patch |
| COS arm64 spot | same | same | + gke-provisioning=spot |
| Ubuntu arm64 on-demand | same | `ubuntu-os-gke-cloud` arm64 | + gke-os-distribution, URL/hash patch |
| Ubuntu arm64 spot | same | same | + gke-provisioning=spot |

### Scenario Tests

- Cluster with org policy `compute.requireShieldedVm`: no Karpenter pools created, nodes register
- Cluster with org policy `gcp.restrictNonCmekServices`: same
- `default-pool` deleted mid-run: selection promotes next candidate automatically
- `DEFAULT_NODEPOOL_TEMPLATE_NAME` set to non-default pool: that pool is used
- All pools temporarily PROVISIONING: Karpenter retries with backoff; provisioning resumes once a pool reaches RUNNING without creating any new pools
- GKE upgrade while Karpenter is running: post-cache-refresh nodes use new template

---

## Acceptance Criteria

The feature is considered complete when:

- Phase 0 credential research is documented and green-lights the approach
- Pool discovery, `PatchKubeEnvForOSType`, and `PatchKubeEnvForArch` are implemented with unit tests
- E2E coverage for all 8 node variants passes on the project e2e cluster
- Open questions 1–2 (NPD pool name, kube-proxy token scope) are validated in e2e; `PatchNodeProblemDetectorConfig` wired in if required
- Old Karpenter-managed pool creation code is removed (no parallel code paths)

---

## Implementation Phases

### Phase 0 — Research: credential viability and pool discovery PoC

Before any implementation begins:

**Credential research** — validate how Group 4 credentials behave when reused across pools:
- **`KUBE_PROXY_TOKEN`**: pool-scoped or cluster-wide? If pool-scoped, can it be safely stripped or self-generated?
- **`TPM_BOOTSTRAP_CERT` / `TPM_BOOTSTRAP_KEY`**: same — pool-scoped or cluster-wide?
- **`NODE_PROBLEM_DETECTOR_ADC_CONFIG`**: does GKE validate the embedded pool name against pool membership? If not, no patch needed. If yes, confirm whether removing the field entirely is safe.
- **GKE cluster API**: do `clusters.get` or related APIs expose any Group 4 credentials directly?

**Prerequisites (blockers before Phase 2)**:
- **`SERVER_BINARY_TAR_HASH` format** (OQ 3): confirm raw hex vs `sha256:`-prefixed against a live kube-env dump before `PatchKubeEnvForArch` can be written.
- **`RENDERED_INSTALLABLES` consistency** (OQ 5): confirm the field is identical across pool types (COS, Ubuntu, mixed) on at least two GKE versions. If it differs, cross-pool reuse of the template is invalid and the approach must be reconsidered.

**Pool discovery PoC**: run a minimal proof-of-concept of pool discovery on the e2e cluster to validate that a node provisioned using a non-owning pool's credentials bootstraps and registers correctly. This gates Phases 1–3.

This phase produces a decision record. If any blocker cannot be resolved, the proposal must be revised before implementation proceeds.

### Phase 0.5 — E2E test coverage baseline

Before any implementation begins, add e2e tests for all node variant scenarios that the subsequent phases will touch. These tests act as a regression gate: they must pass before Phase 0.5 is considered complete, and must continue passing after each subsequent phase.

Scenarios to cover (that are not yet covered or only partially covered):

| Scenario | Why needed |
|---|---|
| Ubuntu amd64 on-demand node provisions and registers | Phase 2 replaces the Ubuntu template pool |
| Ubuntu amd64 spot node provisions and registers | same |
| COS arm64 on-demand node provisions and registers | Phase 2 eliminates the arm64 COS pool |
| Ubuntu arm64 on-demand node provisions and registers | Phase 2 eliminates the arm64 Ubuntu pool |
| Node registers with correct `kubernetes.io/arch` label | Arch patching correctness |
| Node registers with correct `cloud.google.com/machine-family` label | Machine-family patching correctness |
| kube-proxy pod is Running on a Karpenter-provisioned node | Group 4 credential baseline |
| node-problem-detector pod is Running on a Karpenter-provisioned node | NPD credential baseline |
| Cluster with `compute.requireShieldedVm` policy — node provisions | Org-policy regression gate |

These tests run against the project's shared e2e cluster using the existing framework under `test/e2e/`. Env vars `E2E_PROJECT_ID`, `E2E_LOCATION`, and `GOOGLE_APPLICATION_CREDENTIALS` must be set; see `CLAUDE.md` for values.

Tests that require arm64 must guard on `c4a` availability in the configured zone and skip gracefully if unsupported.

### Phase 1 — OS-type patching
Add `PatchKubeEnvForOSType`. Add Ubuntu image resolver (query `ubuntu-os-gke-cloud`, cache by GKE version + arch). Eliminates `karpenter-ubuntu` and `karpenter-ubuntu-arm64` pools.

### Phase 2 — Arm64 hash via GCS sidecar
Add `PatchKubeEnvForArch`. Eliminates `karpenter-cos-arm64` pool. At this point, only `karpenter-default` remains.

### Phase 3 — Pool discovery and selection
Replace `Create()` pool creation with priority-based discovery. Add `DEFAULT_NODEPOOL_TEMPLATE_NAME`. Add `PatchNodeProblemDetectorConfig` if Phase 2 e2e requires it. At this point, 0 Karpenter-specific pools in normal operation.

---

## Migration: Existing Karpenter-Managed Pools

Clusters upgrading from a version that created the four Karpenter template pools will have `karpenter-default`, `karpenter-ubuntu`, `karpenter-cos-arm64`, and `karpenter-ubuntu-arm64` present after the upgrade.

### Should Karpenter delete them automatically?

**No.** Karpenter will not auto-delete legacy pools. They are data-source only and carry zero workload nodes, but auto-deletion on startup introduces risk without benefit. At startup, Karpenter logs the names of any detected legacy Karpenter-managed pools so operators are aware of them. Operators can delete them manually at their own pace once the new version is confirmed stable.

### Rollback consideration

Rolling back to the previous version will re-create the pools on startup if they were already deleted — the same path it follows for a fresh install.

---

## Alternatives Considered

### Derive template config from GCENodeClass

`GCENodeClass` defines compute config for workload nodes. Template pools are bootstrap infrastructure — different concern, different lifecycle. A NodePool can span multiple arches and OS families, making per-NodeClass pool selection unworkable. Rejected.

### Full kube-env reconstruction from GKE APIs with no pool reads

Achievable for Groups 1–3 (all fields available from `clusters.get` or derivable). Blocked on Group 4 (`TPM_BOOTSTRAP_CERT`/`KUBE_PROXY_TOKEN` — generated by GKE per pool, no public API). This proposal is the necessary intermediate step: it validates that Group 4 credentials from an arbitrary pool work for Karpenter nodes, which is a prerequisite for eliminating pool reads entirely.

### Hardcode `default-pool` as the source

`default-pool` is a GKE convention, not a contract. Operators can delete it, never have it (Autopilot → Standard migration), or prefer a different pool. The scoring-based selection algorithm handles all these cases while still preferring `default-pool` when present.

---

## Future Direction

Once this proposal is stable, the remaining dependency on reading any pool template is Group 4 (`TPM_BOOTSTRAP_CERT`, `KUBE_PROXY_TOKEN`). All other groups are either patched in code or available from public GCS endpoints. Possible paths for eventually eliminating pool reads entirely:

- **GKE Confidential Nodes / hardware TPM**: clusters using TPM attestation do not require software bootstrap credentials; Group 4 becomes unnecessary
- **Direct cluster API credentials**: if GKE exposes Group 4 credentials (`TPM_BOOTSTRAP_CERT`, `KUBE_PROXY_TOKEN`) via a cluster-level API, pool reads could be eliminated entirely without any install-time helper
- **GKE bootstrap API**: a future GKE feature exposing per-cluster bootstrap credentials via a privileged API

---

## Open Questions

1. **`NODE_PROBLEM_DETECTOR_ADC_CONFIG` pool name**: ~~Does GKE validate the embedded pool name?~~ **Resolved**: the field embeds the pool name in both audience and credential_source URLs. `PatchNodeProblemDetectorConfig` is required (not optional). Validate in e2e that NPD reaches Running.

2. **`KUBE_PROXY_TOKEN` RBAC scope**: Is the token bound to a pool-scoped RBAC subject? **Partially resolved**: token is cluster-scoped (identical across pools on GKE 1.35.1). Validate in e2e by confirming kube-proxy reaches Running on a node provisioned from a non-owning pool.

3. **`SERVER_BINARY_TAR_HASH` format**: ~~Raw hex or `sha256:`-prefixed?~~ **Resolved**: raw 128-char SHA-512 hex; sidecar file is `.sha512` (not `.sha256`). Verified against GKE 1.35.1.

4. **arm64 COS image source without arm64 pool**: **Resolved**. ARM64 COS images are available in the `gke-node-images` GCP project via `compute.images.list` filtering on `architecture=ARM64`. Family names exist for stable channels (`cos-arm64-stable`, `cos-arm64-lm-125-lts`, etc.) and can be used for version-agnostic image selection. GKE-version-pinned images (pattern `gke-{version}-gke{build}-cos-arm64-...`) have **no family name** and must be selected by matching the image name against the GKE cluster version. Strategy for `PatchKubeEnvForArch` when provisioning COS arm64: query `gke-node-images` by `architecture=ARM64` + GKE version prefix in the image name, pick the newest matching image.

5. **`RENDERED_INSTALLABLES` consistency**: ~~Verify holds across OS types and GKE versions.~~ **Resolved for OQ**: identical between COS and Ubuntu pools on GKE 1.35.1 (byte-for-byte diff clean). Cross-version confirmation still recommended before Stable.

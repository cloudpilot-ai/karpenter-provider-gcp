# Proposal: Eliminate Karpenter-Managed Node Pool Dependency

- **Status**: Provisional
- **Authors**: @dm3ch
- **Created**: 2026-04-17
- **Related Issues**: [#255](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/255)

---

## Summary

Karpenter today creates and manages up to four GKE node pools (`karpenter-default`, `karpenter-ubuntu`, `karpenter-cos-arm64`, `karpenter-ubuntu-arm64`) solely to read back the bootstrap metadata (kube-env) that GKE generates for them. This design fails in environments with org policies like `constraints/compute.requireShieldedVm` or `constraints/gcp.restrictNonCmekServices` because Karpenter lacks the configuration to create policy-compliant pools.

This proposal replaces that approach in two stages:

1. **Stage 2** (this proposal): Stop creating pools. Read bootstrap metadata from whichever amd64 node pool already exists in the cluster, using a scoring-based selection algorithm. Eliminate the arm64-specific pool requirement by fetching the architecture-specific binary hash from a publicly available GCS sidecar file. Fall back to creating one pool only if the cluster has no existing amd64 pool at all.

2. **Stage 3** (future work, scoped here but not implemented): Assemble kube-env entirely from GKE APIs and public GCS endpoints without reading any pool at all.

---

## Motivation

### Problem Statement

The current architecture creates node pools as a workaround to obtain three categories of data that GKE embeds in pool instance templates:

- **Cluster-level constants** (CA certificate, master endpoint, IP ranges, feature flags) — identical across all pools, available from `clusters.get`
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
- Graceful fallback when no suitable pool exists.

### Non-Goals

- Full kube-env reconstruction from GKE APIs alone (Stage 3 — future work).
- Changes to the `GCENodeClass` or `NodePool` API surface.
- Support for Windows nodes (not currently supported regardless of this change).
- Removal of the fallback pool-creation code path.

---

## Proposal

### Overview

The core change is in `pkg/providers/nodepooltemplate/`: replace the four hardcoded pool creation calls with a pool discovery and selection algorithm. All downstream code (instance building, metadata patching) is unchanged except for two new patch functions.

```
Before:
  Karpenter creates pools → reads back templates → provisions nodes

After:
  Karpenter discovers existing pools → selects best → provisions nodes
  (fallback: creates one pool if none exists)
```

### Bootstrap Data Sources After This Change

The table below maps each kube-env field group to its source in Stage 2. "Any amd64 pool" means whichever pool the selection algorithm picks — not necessarily `default-pool`.

| Group | Fields | Stage 2 source |
|---|---|---|
| 1 — Cluster constants | CA cert, master endpoint, IP ranges, feature flags, … | Any amd64 pool's template (Stage 3: `clusters.get`) |
| 2 — Arch-specific binary | `SERVER_BINARY_TAR_URL`, `SERVER_BINARY_TAR_HASH` | amd64 from pool; arm64 URL derived, hash from GCS `.sha256` sidecar |
| 3 — OS-specific | `gke-os-distribution`, BFQ scheduler flags | Patched at provisioning time (`PatchKubeEnvForOSType`) |
| 4 — Per-pool credentials | `TPM_BOOTSTRAP_CERT`, `KUBE_PROXY_TOKEN`, NPD config | Any amd64 pool's template |
| 5 — Node-specific | arch label, machine-family, provisioning model, max-pods | Patched at provisioning time (existing functions, no change) |
| 6 — Boot images | Source image URL per OS + arch | COS: pool template boot disk; Ubuntu: `ubuntu-os-gke-cloud` image catalog |

### Design Details

#### Pool Selection Algorithm

On startup and on each template refresh cycle, Karpenter enumerates all node pools in the cluster and selects one as the bootstrap source:

```
1. If DEFAULT_NODEPOOL_TEMPLATE_NAME is set → use that pool; error if not RUNNING.
2. List all pools: containerService.NodePools.List(cluster)
3. Filter: status == RUNNING, machine type not arm64 (not c4a-*, t2a-*, a4x-*)
4. Score remaining pools:
     default-pool                           → 10
     COS_CONTAINERD image type, not arm64   →  5
     any other amd64 pool                   →  1
5. Pick highest score. On tie, sort by name (deterministic).
6. Fallback: if no candidates → create karpenter-default (existing code path)
```

The selected pool name is stored in the provider struct, refreshed every sync cycle. `GetInstanceTemplates()` returns a single template keyed by the selected pool name.

#### Architecture-Specific Binary Metadata (Group 2)

GCS publishes `.sha256` sidecar files alongside every GKE release tarball:

```
https://storage.googleapis.com/gke-release/kubernetes/release/{VERSION}/
  kubernetes-server-linux-{ARCH}.tar.gz        ← ~100 MB tarball
  kubernetes-server-linux-{ARCH}.tar.gz.sha256 ← 65 bytes, SHA-256 hex
```

Confirmed on all three mirrors (`gke-release`, `gke-release-eu`, `gke-release-asia`).

For arm64 nodes, `PatchKubeEnvForArch` (new function in `pkg/providers/metadata/utils.go`):

1. Parses `SERVER_BINARY_TAR_URL` lines from kube-env (amd64 URLs)
2. Substitutes `linux-amd64` → `linux-arm64` in each mirror URL
3. Extracts GKE version from the URL path
4. Fetches `{primary_url}.sha256` (65 bytes) using an HTTP client; caches by version string in a `sync.Map`
5. Replaces both `SERVER_BINARY_TAR_URL` and `SERVER_BINARY_TAR_HASH` in kube-env

> **Pre-implementation check**: Verify the exact format of `SERVER_BINARY_TAR_HASH` in a live kube-env before writing the replacement regex (raw hex vs `sha256:`-prefixed).

This approach is structurally equivalent to AWS Karpenter's use of SSM Parameter Store for per-arch AMI IDs — a "query a known public endpoint by version and arch" pattern rather than creating a template resource.

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

`TPM_BOOTSTRAP_CERT`, `TPM_BOOTSTRAP_KEY`, and `KUBE_PROXY_TOKEN` are unique per pool but not per cluster member type. Evidence that cross-pool reuse works:

- Karpenter today uses `karpenter-default`'s credentials for both COS and Ubuntu nodes (different OS, same pool credentials). No bootstrap failures observed.
- Karpenter VMs join the cluster via the Compute API, not through the GKE node pool API. They are not pool members and GKE does not enforce pool membership at bootstrap time.

`NODE_PROBLEM_DETECTOR_ADC_CONFIG` contains the source pool name in its audience URL. If GKE validates this, NPD authentication will fail on Karpenter-provisioned nodes. `PatchNodeProblemDetectorConfig` (new function) replaces the embedded pool name with a stable synthetic value. This will be validated in e2e and wired in if confirmed necessary.

#### Code Changes

| File | Change |
|---|---|
| `pkg/providers/nodepooltemplate/nodepooltemplate.go` | `Create()` → no-op unless no RUNNING amd64 pool found (fallback only). Add `discoverSourcePool()` with scoring algorithm. |
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
- The refresh cycle re-runs selection. If another RUNNING amd64 pool exists, it is promoted automatically with no operator action.
- If no pool remains, the fallback creates `karpenter-default` — same as the current primary path.
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

**Scenario**: Cluster has several RUNNING amd64 pools, each potentially with different custom metadata or image settings.

**Impact**: Group 1 (cluster constants) and Group 4 (credentials) are identical across pools — verified across four pools on a live GKE 1.35.1 cluster. Custom `Properties.Metadata` keys set by operators are the only divergence risk.

**Mitigation**: The scoring algorithm is deterministic (stable sort by name on tie). Operators who need a specific pool can pin it via `DEFAULT_NODEPOOL_TEMPLATE_NAME`. Document this option clearly.

### `NODE_PROBLEM_DETECTOR_ADC_CONFIG` pool name validation

**Scenario**: GKE validates that the audience URL's embedded pool name corresponds to a pool the node is a member of. Karpenter nodes using another pool's NPD config fail authentication.

**Likelihood**: Low — Karpenter already cross-uses credentials for different OS families without NPD failures.

**Mitigation**: `PatchNodeProblemDetectorConfig` replaces the embedded name with a stable synthetic value (`karpenter`). Wire in after e2e validation of NPD pod status on provisioned nodes.

### `KUBE_PROXY_TOKEN` pool-scoped RBAC

**Scenario**: GKE issues the token against a pool-scoped RBAC subject. Using another pool's token causes kube-proxy auth failures.

**Likelihood**: Low — no reported failures in current Karpenter operation where one pool's token is used for a different OS family.

**Mitigation**: Validate in e2e by inspecting kube-proxy pod logs on Karpenter-provisioned nodes using a non-owning pool's credentials.

### No RUNNING amd64 pool in cluster

**Scenario**: A cluster with only arm64 node pools, or a freshly drained cluster.

**Mitigation**: Fallback creates `karpenter-default` (lightweight COS amd64 pool, 0 nodes). This is the current primary path — well-tested and unchanged.

---

## Test Plan

### Unit Tests

- `discoverSourcePool` selection: mock pool lists with various combinations (no pools, only arm64, `default-pool` present, tie between equal-score pools, `DEFAULT_NODEPOOL_TEMPLATE_NAME` set)
- `PatchKubeEnvForOSType`: COS→Ubuntu OS distribution replacement, BFQ field removal, no-op on COS input
- `PatchKubeEnvForArch`: URL substitution, hash replacement from mock HTTP server returning `.sha256` content, cache hit avoids second HTTP call
- `PatchNodeProblemDetectorConfig`: pool name replacement in audience URL, no-op when field absent

### E2E Test Matrix

| Variant | Bootstrap source | Image source | Patches |
|---|---|---|---|
| COS amd64 on-demand | discovered amd64 pool | pool template boot disk | arch, machine-family, provisioning |
| COS amd64 spot | same | same | + gke-provisioning=spot |
| Ubuntu amd64 on-demand | same | `ubuntu-os-gke-cloud` lookup | + gke-os-distribution, -BFQ |
| Ubuntu amd64 spot | same | same | + gke-provisioning=spot |
| COS arm64 on-demand | same (amd64 pool) | `gke-node-images` arm64 | arch=arm64, URL/hash patch |
| COS arm64 spot | same | same | + gke-provisioning=spot |
| Ubuntu arm64 on-demand | same (amd64 pool) | `ubuntu-os-gke-cloud` arm64 | + gke-os-distribution, URL/hash patch |
| Ubuntu arm64 spot | same | same | + gke-provisioning=spot |

### Scenario Tests

- Cluster with org policy `compute.requireShieldedVm`: no Karpenter pools created, nodes register
- Cluster with org policy `gcp.restrictNonCmekServices`: same
- `default-pool` deleted mid-run: selection promotes next candidate automatically
- `DEFAULT_NODEPOOL_TEMPLATE_NAME` set to non-default pool: that pool is used
- No RUNNING amd64 pool: fallback creates `karpenter-default`, provisioning succeeds
- GKE upgrade while Karpenter is running: post-cache-refresh nodes use new template

---

## Graduation Criteria

**Alpha** (implementation complete, feature-gated or opt-in):
- Pool discovery and selection implemented and passing unit tests
- `PatchKubeEnvForArch` (Group 2), `PatchKubeEnvForOSType` (Group 3) implemented and tested
- E2E coverage for all 8 node variants above on `dm3ch-karpenter-dev`

**Beta** (default behavior, existing pool-creation retained as fallback):
- Open questions 1–2 (NPD pool name, kube-proxy token scope) validated in e2e
- `PatchNodeProblemDetectorConfig` wired in if required
- No regressions in existing e2e suite

**Stable** (pool creation code path fully deprecated):
- Two release cycles of Beta with no reported regressions
- Document the fallback behavior in user-facing docs

---

## Implementation Phases

### Phase 0 — E2E test coverage baseline

Before any implementation begins, add e2e tests for all node variant scenarios that the subsequent phases will touch. These tests act as a regression gate: they must pass before Phase 0 is considered complete, and must continue passing after each subsequent phase.

Scenarios to cover (that are not yet covered or only partially covered):

| Scenario | Why needed |
|---|---|
| Ubuntu amd64 on-demand node provisions and registers | Phase 2 replaces the Ubuntu template pool |
| Ubuntu amd64 spot node provisions and registers | same |
| COS arm64 on-demand node provisions and registers | Phase 3 eliminates the arm64 COS pool |
| Ubuntu arm64 on-demand node provisions and registers | Phase 3 eliminates the arm64 Ubuntu pool |
| Node registers with correct `kubernetes.io/arch` label | Arch patching correctness |
| Node registers with correct `cloud.google.com/machine-family` label | Machine-family patching correctness |
| kube-proxy pod is Running on a Karpenter-provisioned node | Group 4 credential baseline |
| node-problem-detector pod is Running on a Karpenter-provisioned node | NPD credential baseline |
| Cluster with `compute.requireShieldedVm` policy — node provisions | Org-policy regression gate |

These tests run against `dm3ch-karpenter-dev` (`us-central1-f`) using the existing e2e framework. Tests that require arm64 must be skipped if the zone does not support arm64 machine types (guard on `c4a` availability).

### Phase 1 — Prerequisite (done / in progress)
Merge PR #253 as stop-gap for org-policy failures.

### Phase 2 — OS-type patching
Add `PatchKubeEnvForOSType`. Add Ubuntu image resolver (query `ubuntu-os-gke-cloud`, cache by GKE version + arch). Eliminates `karpenter-ubuntu` and `karpenter-ubuntu-arm64` pools.

### Phase 3 — Arm64 hash via GCS sidecar
Add `PatchKubeEnvForArch`. Eliminates `karpenter-cos-arm64` pool. At this point, only `karpenter-default` remains.

### Phase 4 — Pool discovery and selection (Stage 2 target)
Replace `Create()` pool creation with scoring-based discovery. Add `DEFAULT_NODEPOOL_TEMPLATE_NAME`. Add `PatchNodeProblemDetectorConfig` if Phase 3 e2e requires it. At this point, 0 Karpenter-specific pools in normal operation.

---

## Alternatives Considered

### Controller-level env vars for org-policy fields
_(PR #253)_

Extends the `DEFAULT_NODEPOOL_SERVICE_ACCOUNT` pattern to CMEK key and Shielded VM. Unblocks users but does not reduce pool count. CMEK and Shielded VM are org-policy concerns that should not require Karpenter Helm values. Accepted as stop-gap; not the target shape.

### Derive template config from GCENodeClass

`GCENodeClass` defines compute config for workload nodes. Template pools are bootstrap infrastructure — different concern, different lifecycle. A NodePool can span multiple arches and OS families, making per-NodeClass pool selection unworkable. Rejected.

### Full kube-env reconstruction from GKE APIs (skip Stage 2)

Achievable today for Groups 1–3 (all fields available from `clusters.get` or derivable). Blocked on Group 4 (`TPM_BOOTSTRAP_CERT`/`KUBE_PROXY_TOKEN` — generated by GKE per pool, no public API). Stage 2 is the necessary intermediate step: it validates that Group 4 credentials from an arbitrary pool work for Karpenter nodes, which is the prerequisite for Stage 3.

### Hardcode `default-pool` as the source

`default-pool` is a GKE convention, not a contract. Operators can delete it, never have it (Autopilot → Standard migration), or prefer a different pool. The scoring-based selection algorithm handles all these cases while still preferring `default-pool` when present.

---

## Stage 3: Future Work

After Stage 2 validates Group 4 credential reuse, Stage 3 eliminates the remaining dependency on reading any pool at all.

**What Stage 3 requires**:
- Source Groups 1 and 3 from `clusters.get` directly (already mapped; all fields available)
- Source Group 2 (arm64) from GCS `.sha256` sidecars (already resolved in Phase 3)
- Source Group 4 credentials by one of:
  - **Static install-time config** (Azure Karpenter pattern): `configure-values.sh` reads credentials from a cluster pool once at install and puts them in Helm values. Requires credential rotation on pool recreation.
  - **GKE Confidential Nodes / hardware TPM**: clusters using TPM attestation do not require software bootstrap credentials. Karpenter detects this and skips Group 4. No Helm changes needed.
  - **GKE bootstrap API** (feature request): GKE exposes per-cluster bootstrap credentials via a privileged API.

Stage 3 target state: `clusters.get` + GCS `.sha256` sidecars + GKE image catalogs → kube-env assembled in memory → node provisioned with no pool read.

---

## Open Questions

1. **`NODE_PROBLEM_DETECTOR_ADC_CONFIG` pool name**: Does GKE validate the embedded pool name in the audience URL against node pool membership? Validate in e2e (Phase 3). Patch function ready to wire in.

2. **`KUBE_PROXY_TOKEN` RBAC scope**: Is the token bound to a pool-scoped RBAC subject? Validate by checking kube-proxy logs on Karpenter nodes provisioned using a non-owning pool's credentials.

3. **`SERVER_BINARY_TAR_HASH` format**: Raw 64-char hex or `sha256:`-prefixed? Must be confirmed against a live kube-env dump before writing the replacement regex in `PatchKubeEnvForArch`.

4. **arm64 COS image source without arm64 pool**: Can `gke-node-images` project provide arm64 COS images via `compute.images.list` filtering on architecture? Or must the image URL be derived from an arm64 pool's boot disk? Needs API testing.

5. **`RENDERED_INSTALLABLES` consistency**: Verified identical across 4 pools on a single GKE 1.35.1 cluster. Confirm this holds across diverse cluster configurations (Autopilot-converted, custom node images, etc.).

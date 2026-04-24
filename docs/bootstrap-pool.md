# Bootstrap Pool Selection

Karpenter requires a RUNNING GKE node pool at startup to read bootstrap metadata (kubelet configuration, network settings, instance template). This page describes how that pool is selected and how to control it.

## Selection algorithm

When Karpenter starts, it evaluates all node pools in the cluster and selects the bootstrap source using this priority order:

1. The pool named by `DEFAULT_NODEPOOL_TEMPLATE_NAME` (if set and eligible)
2. The pool named `default-pool` (if eligible)
3. The first eligible pool in alphabetical order

A pool is eligible when its status is `RUNNING` or `RUNNING_WITH_ERROR`.

If no eligible pool is found, Karpenter creates a minimal fallback pool named `karpenter-fallback` (COS, amd64, zero nodes) and retries. The fallback pool is hardened against common org policy constraints:

- `compute.requireShieldedVm` — Shielded VM (Secure Boot + Integrity Monitoring) always enabled
- `container.managed.enablePrivateNodes` — mirrored from cluster config automatically
- `container.managed.enableWorkloadIdentityFederation` — GKE\_METADATA mode set when Workload Identity is active on the cluster
- `compute.managed.blockProjectSshKeys` — SSH key blocking always set in node metadata

Karpenter does **not** create pools proactively. The fallback is only a last resort.

## Pinning the bootstrap pool

On clusters where org policies prevent pool creation (e.g. `gcp.restrictNonCmekServices`), pre-create a RUNNING pool and pin it explicitly:

```yaml
controller:
  settings:
    defaultNodePoolTemplateName: "my-existing-pool"
```

When set, Karpenter uses this pool exclusively and returns an error if it is not RUNNING, making pool selection deterministic and independent of cluster pool order.

## Legacy pools

Older Karpenter versions created pools named `karpenter-ubuntu`, `karpenter-cos-arm64`, and `karpenter-ubuntu-arm64`. These are no longer created. See [MIGRATION.md](../MIGRATION.md) for cleanup steps.

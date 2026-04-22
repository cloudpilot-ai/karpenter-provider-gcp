# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),

## Unreleased

### Breaking Changes

- Karpenter no longer creates `karpenter-default`, `karpenter-ubuntu`, `karpenter-cos-arm64`,
  or `karpenter-ubuntu-arm64` GKE node pools on startup. Bootstrap metadata is now read from
  an existing RUNNING cluster pool selected by the new pool discovery algorithm. See
  [MIGRATION.md](MIGRATION.md) for cleanup steps.

### New features

- **Eliminate template node pool dependency** (#255): Karpenter now discovers an existing
  RUNNING cluster pool as its bootstrap source instead of creating dedicated template pools.
  This unblocks clusters under org policies such as `compute.requireShieldedVm` or
  `gcp.restrictNonCmekServices` that prevented Karpenter from creating pools.
  - Ubuntu and arm64 nodes are now provisioned by patching the kube-env from any COS source
    pool, eliminating the need for OS- or arch-specific template pools.
  - A last-resort `karpenter-default` pool (with shielded VM options enabled) is created only
    when no RUNNING pool is available.
  - The bootstrap source pool can be pinned by setting the `DEFAULT_NODEPOOL_TEMPLATE_NAME`
    env var (or `--default-nodepool-template-name` flag).
  - Bootstrap source pool selection is logged at INFO level when the pool changes; a
    `karpenter_gcp_bootstrap_source_pool` Prometheus gauge is planned for a future release.

- Added a garbage-collection controller (`instance.garbagecollection`) that periodically
  detects and deletes GCE VM instances with no corresponding NodeClaim, preventing orphaned
  instances from accumulating after missed deletes or controller restarts.
- Added `goog-k8s-cluster-location` GCE label to instances at creation time. Combined with
  the existing `goog-k8s-cluster-name` label, the GC controller can now scope deletions to
  the correct cluster when multiple clusters share the same name in different GCP locations.
  Instances without the label (created by an older Karpenter version) remain in the instance
  cache so Karpenter retains full control of them, but are never touched by the GC
  controller. The label will be enforced strictly in a future release once all clusters have
  migrated.

No action is required to upgrade. Existing nodes stay fully managed until they are
naturally replaced by the new version, at which point they receive the location label and
become eligible for automatic GC. See [MIGRATION.md](MIGRATION.md) for optional steps to
rotate nodes early, clean up legacy template pools, and delete any orphaned instances that
may have accumulated before this release.

## v0.2.0

### Breaking Changes

- `karpenter.k8s.gcp/instance-family` requirements now match the **machine type prefix** (the part before the first `-`), not the full family+shape.
  - **Before** (worked): `["n4-standard", "n2-standard"]`
  - **Now** (required): `["n4", "n2"]`
  - `e2` is unchanged (e.g. `e2-medium` still has family `e2`).

If you upgrade to v0.2.0 and Karpenter suddenly stops finding instance types, check any existing NodePools that constrain `karpenter.k8s.gcp/instance-family` and update values accordingly.

### Upgrade guide

Update any NodePool `requirements` that use `karpenter.k8s.gcp/instance-family` to use the prefix (e.g. `n4`, `n2`), not the combined family+shape (e.g. `n4-standard`, `n2-standard`).

Before:

```yaml
requirements:
  - key: "karpenter.k8s.gcp/instance-family"
    operator: In
    values: ["n4-standard", "n2-standard", "e2"]
```

After:

```yaml
requirements:
  - key: "karpenter.k8s.gcp/instance-family"
    operator: In
    values: ["n4", "n2", "e2"]
```

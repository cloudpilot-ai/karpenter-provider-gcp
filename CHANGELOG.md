# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),

## Unreleased

### New features

- Added a garbage-collection controller (`instance.garbagecollection`) that periodically
  detects and deletes GCE VM instances with no corresponding NodeClaim, preventing orphaned
  instances from accumulating after missed deletes or controller restarts.
- Added `goog-k8s-cluster-location` GCE label to instances at creation time. Combined with
  the existing `goog-k8s-cluster-name` label, Karpenter now scopes the instance cache and
  GC controller to the correct cluster even when multiple clusters share the same name in
  different GCP locations. Instances without `goog-k8s-cluster-location` are excluded from
  the cache and are not touched by the GC controller.

### Migration guide

No action is required to upgrade. See [MIGRATION.md](MIGRATION.md) for optional steps to
rotate existing nodes onto the new label scheme and clean up any orphaned instances that
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

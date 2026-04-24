# Upgrading

This guide explains how to safely upgrade Karpenter GCP to a new version.

## Standard upgrade procedure

Always upgrade CRDs before the controller. Helm's `helm upgrade` never upgrades CRDs that already exist — the separate `karpenter-crd` chart is the supported mechanism for keeping CRDs current.

```sh
helm repo update

# 1. Upgrade CRDs
helm upgrade karpenter-crd karpenter-provider-gcp/karpenter-crd

# 2. Upgrade the controller
helm upgrade karpenter karpenter-provider-gcp/karpenter \
  --namespace karpenter-system
```

If you installed with the single-chart pattern before `karpenter-crd` was available, migrate by installing the CRD chart once:

```sh
helm upgrade --install karpenter-crd karpenter-provider-gcp/karpenter-crd
```

Subsequent upgrades use the two-step procedure above.

## Version-specific migration notes

See [MIGRATION.md](../../MIGRATION.md) for migration notes that require manual steps.

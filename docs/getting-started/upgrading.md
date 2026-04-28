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

If you installed with the single-chart pattern before `karpenter-crd` was available, the in-cluster CRDs are not currently owned by any Helm release. Adopt them into the new `karpenter-crd` release first, so Helm doesn't error with `cannot patch <crd> ... already exists`:

```bash
for crd in gcenodeclasses.karpenter.k8s.gcp \
           nodeclaims.karpenter.sh \
           nodeoverlays.karpenter.sh \
           nodepools.karpenter.sh; do
  kubectl annotate crd $crd \
    meta.helm.sh/release-name=karpenter-crd \
    meta.helm.sh/release-namespace=karpenter-system \
    --overwrite
  kubectl label crd $crd app.kubernetes.io/managed-by=Helm --overwrite
done
```

Then install the CRD chart:

```sh
helm upgrade --install karpenter-crd karpenter-provider-gcp/karpenter-crd
```

Subsequent upgrades use the two-step procedure above.

## Version-specific migration notes

See [MIGRATION.md](../../MIGRATION.md) for migration notes that require manual steps.

# Upgrading

This guide explains how to safely upgrade Karpenter GCP to a new version.

## Standard upgrade procedure

Always upgrade CRDs before the controller. Helm's `helm upgrade` never upgrades CRDs that already exist — the separate `karpenter-crd` chart is the supported mechanism for keeping CRDs current.

New releases require Helm 3.8+ or Helm 4 and use public GHCR OCI charts. Choose an explicit release version for both charts; `helm repo update` is not needed. Existing Helm release names and values do not change when switching chart sources. Preserve your configured values with `-f` or the same `--set` options used for installation.

```sh
export CHART_VERSION=X.Y.Z

# 1. Upgrade CRDs
helm upgrade karpenter-crd oci://ghcr.io/cloudpilot-ai/karpenter-provider-gcp/charts/karpenter-crd \
  --version "$CHART_VERSION"

# 2. Upgrade the controller
helm upgrade karpenter oci://ghcr.io/cloudpilot-ai/karpenter-provider-gcp/charts/karpenter \
  --version "$CHART_VERSION" --namespace karpenter-system -f your-values.yaml
```

The old Pages repository is frozen for [historical releases](installation.md#historical-releases); future versions are OCI-only. Helm OCI versions support SemVer constraints, for example `--version '>=0.9.0 <0.10.0'` for stable `0.9.x` charts. A range without a prerelease comparator excludes release candidates; select an exact `X.Y.Z-rc.N` or use a prerelease-aware range such as `>=0.9.0-0 <0.10.0` when testing.

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
helm upgrade --install karpenter-crd oci://ghcr.io/cloudpilot-ai/karpenter-provider-gcp/charts/karpenter-crd \
  --version "$CHART_VERSION"
```

Subsequent upgrades use the two-step procedure above.

## Version-specific migration notes

See [MIGRATION.md](../../MIGRATION.md) for migration notes that require manual steps.

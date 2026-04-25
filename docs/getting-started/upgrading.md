# Upgrading

## Migrating to chart `<NEXT-VERSION>` (CRDs moved to a separate chart)

Starting in chart `<NEXT-VERSION>`, the four Karpenter CRDs ship via a companion chart `karpenter-crd` instead of the `crds/` directory of the main `karpenter` chart. This makes CRD upgrades possible via standard `helm upgrade` (Helm doesn't upgrade resources in `crds/` after install).

If this is a fresh cluster, install both charts in order — see [Installation](./installation.md).

If you have an existing install, the in-cluster CRDs were created by Helm as install-only resources of the previous `karpenter` release and are not currently owned by any release. Adopt them into the new `karpenter-crd` release before installing it, so Helm doesn't error with `cannot patch … already exists`:

```bash
for crd in gcenodeclasses.karpenter.k8s.gcp \
           nodeclaims.karpenter.sh \
           nodeoverlays.karpenter.sh \
           nodepools.karpenter.sh; do
  kubectl annotate crd $crd \
    meta.helm.sh/release-name=karpenter-crd \
    meta.helm.sh/release-namespace=<karpenter-namespace> \
    --overwrite
  kubectl label crd $crd app.kubernetes.io/managed-by=Helm --overwrite
done
```

Then install the companion chart at the same version as your main chart, followed by upgrading the main chart:

```bash
helm repo add karpenter-provider-gcp https://cloudpilot-ai.github.io/karpenter-provider-gcp
helm repo update

helm install karpenter-crd karpenter-provider-gcp/karpenter-crd \
  --version "<NEXT-VERSION>" \
  --namespace <karpenter-namespace>

helm upgrade karpenter karpenter-provider-gcp/karpenter \
  --version "<NEXT-VERSION>" \
  --namespace <karpenter-namespace>
```

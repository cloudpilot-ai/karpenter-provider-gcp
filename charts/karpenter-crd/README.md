# karpenter-crd

Karpenter GCP CRD chart - installs and upgrades Karpenter CustomResourceDefinitions independently

The `karpenter-crd` chart installs and upgrades the Karpenter GCP CRDs independently
of the controller chart. Installing CRDs via this chart allows `helm upgrade karpenter-crd`
to safely upgrade CRDs before rolling out a new controller version.

## Why a separate CRD chart?

Helm's `helm upgrade` deliberately never upgrades CRDs that already exist in the cluster.
The standard workaround is a dedicated CRD chart whose resources live in `templates/`
(not `crds/`) so that `helm upgrade` applies them normally.

## Installation

Install CRDs first:

```bash
# Requires Helm 3.8+ or Helm 4; set the same --version X.Y.Z on both charts.
helm upgrade --install karpenter-crd oci://ghcr.io/cloudpilot-ai/karpenter-provider-gcp/charts/karpenter-crd
```

Then install the controller:

```bash
helm upgrade --install karpenter oci://ghcr.io/cloudpilot-ai/karpenter-provider-gcp/charts/karpenter \
  --namespace karpenter-system --create-namespace \
  --set "controller.settings.projectID=<project>" \
  --set "controller.settings.clusterName=<cluster>" \
  --set "controller.settings.clusterLocation=<region-or-zone>"
```

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| additionalAnnotations | object | `{}` | Additional annotations to add to all CRD resources. |

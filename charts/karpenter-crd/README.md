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
helm repo add karpenter-provider-gcp https://cloudpilot-ai.github.io/karpenter-provider-gcp
helm repo update

helm upgrade --install karpenter-crd karpenter-provider-gcp/karpenter-crd
```

Then install the controller:

```bash
helm upgrade --install karpenter karpenter-provider-gcp/karpenter \
  --namespace karpenter-system --create-namespace \
  --set "controller.settings.projectID=<project>" \
  --set "controller.settings.clusterName=<cluster>" \
  --set "controller.settings.clusterLocation=<region-or-zone>"
```

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| additionalAnnotations | object | `{}` | Additional annotations to add to all CRD resources. |

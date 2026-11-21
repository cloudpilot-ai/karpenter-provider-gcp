# Ubuntu nodes

Use Ubuntu instead of Container-Optimized OS when workloads require a general-purpose Linux distribution. `osVersion` is required: Karpenter never picks an Ubuntu release for you. The older `Ubuntu2404`/`Ubuntu2204` families still work but are deprecated.

See [`examples/nodeclass/ubuntu-gcenodeclass.yaml`](https://github.com/karpenter-gcp/karpenter-provider-gcp/blob/main/examples/nodeclass/ubuntu-gcenodeclass.yaml).

## Ubuntu 26.04

Ubuntu 26.04 images exist for Kubernetes 1.37 and later minors. Check [available versions](../image-management.md#finding-available-versions) for your cluster's minor first.

```yaml
imageSelectorTerms:
  - family: Ubuntu
    osVersion: "26.04"
    version: latest
```

## Ubuntu 24.04

```yaml
imageSelectorTerms:
  - family: Ubuntu
    osVersion: "24.04"
    version: latest
```

## Ubuntu 22.04

GKE publishes Ubuntu 22.04 images only through Kubernetes 1.31.

```yaml
imageSelectorTerms:
  - family: Ubuntu
    osVersion: "22.04"
    version: latest
```

## Pinned version

Pin to a specific Ubuntu build using the date format `vYYYYMMDD`. If the build has a trailing release letter, include it (for example, `v20251218a`); pins match exactly:

```yaml
imageSelectorTerms:
  - family: Ubuntu
    osVersion: "24.04"
    version: "v20260416"
```

See [Image selection](../image-selection.md) for details on the `family` and `version` fields.

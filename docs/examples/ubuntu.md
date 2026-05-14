# Ubuntu nodes

Use Ubuntu instead of Container-Optimized OS when workloads require a general-purpose Linux distribution.

See [`examples/nodeclass/ubuntu-gcenodeclass.yaml`](https://github.com/cloudpilot-ai/karpenter-provider-gcp/blob/main/examples/nodeclass/ubuntu-gcenodeclass.yaml).

## Ubuntu 24.04

```yaml
imageSelectorTerms:
  - family: Ubuntu2404
    version: latest
```

## Ubuntu 22.04

```yaml
imageSelectorTerms:
  - family: Ubuntu2204
    version: latest
```

## Pinned version

Pin to a specific Ubuntu build using the date format `vYYYYMMDD`:

```yaml
imageSelectorTerms:
  - family: Ubuntu2404
    version: "v20260416"
```

See [Image selection](../image-selection.md) for details on the `family` and `version` fields.

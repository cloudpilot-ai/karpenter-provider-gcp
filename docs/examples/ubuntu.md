# Ubuntu nodes

Use Ubuntu instead of Container-Optimized OS when workloads require a general-purpose Linux distribution.

See [`examples/nodeclass/ubuntu-gcenodeclass.yaml`](https://github.com/cloudpilot-ai/karpenter-provider-gcp/blob/main/examples/nodeclass/ubuntu-gcenodeclass.yaml).

To pin a specific Ubuntu version:

```yaml
imageSelectorTerms:
  - alias: Ubuntu@v20250106
```

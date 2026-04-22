# Static capacity (fixed node count)

A NodePool with `spec.replicas` set maintains a fixed number of nodes regardless of pod demand. Karpenter provisions nodes up to that count at startup and replaces them if they are removed. Consolidation and `consolidateAfter` are ignored on static NodePools.

This feature is disabled by default and requires the `staticCapacity` feature gate.

> **Alpha:** This feature is controlled by the `staticCapacity` feature gate. Alpha features are off by default, may have known limitations, and their behaviour may change in future releases.

## Enabling the feature gate

```sh
helm upgrade karpenter karpenter-provider-gcp/karpenter --install \
  --namespace karpenter-system \
  --set "controller.featureGates.staticCapacity=true" \
  ...
```

Or in `values.yaml`:

```yaml
controller:
  featureGates:
    staticCapacity: true
```

## Example: always-on warm pool

Keep three on-demand nodes running at all times to absorb burst traffic without cold-start latency.

See [`examples/nodepool/static-capacity-nodepool.yaml`](https://github.com/cloudpilot-ai/karpenter-provider-gcp/blob/main/examples/nodepool/static-capacity-nodepool.yaml).

## Combining static and dynamic pools

Static and dynamic NodePools can coexist. Assign weights so that dynamic pools fill first and the static pool acts as a reserved baseline:

```yaml
# Dynamic pool — fills on demand
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: dynamic
spec:
  weight: 10
  template:
    spec:
      nodeClassRef:
        group: karpenter.k8s.gcp
        kind: GCENodeClass
        name: default-example
      requirements:
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["spot", "on-demand"]
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    consolidateAfter: 0s
---
# Static pool — always-on baseline
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: baseline
spec:
  replicas: 2
  weight: 100
  template:
    spec:
      nodeClassRef:
        group: karpenter.k8s.gcp
        kind: GCENodeClass
        name: default-example
      requirements:
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["on-demand"]
  disruption:
    consolidationPolicy: WhenEmpty
    consolidateAfter: 0s
```

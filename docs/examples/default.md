# Default NodePool examples

Standalone YAML files for the examples below are in [`examples/`](https://github.com/cloudpilot-ai/karpenter-provider-gcp/tree/main/examples) and can be applied directly.

## Spot and on-demand, multi-zone

A general-purpose pool that selects spot instances when available and falls back to on-demand.

See [`examples/nodeclass/default-gcenodeclass.yaml`](https://github.com/cloudpilot-ai/karpenter-provider-gcp/blob/main/examples/nodeclass/default-gcenodeclass.yaml) and [`examples/nodepool/default-nodepool.yaml`](https://github.com/cloudpilot-ai/karpenter-provider-gcp/blob/main/examples/nodepool/default-nodepool.yaml).

## On-demand only

Disable spot to avoid preemption for sensitive workloads.

```yaml
requirements:
  - key: karpenter.sh/capacity-type
    operator: In
    values: ["on-demand"]
  - key: karpenter.k8s.gcp/instance-family
    operator: In
    values: ["n2", "n4"]
  - key: kubernetes.io/arch
    operator: In
    values: ["amd64"]
```

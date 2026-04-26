# GPU workloads

Request GPU resources via `nvidia.com/gpu` and select GPU-equipped instance families.

Set `autoGPUTaint: true` to automatically taint GPU nodes with `nvidia.com/gpu=present:NoSchedule`, matching GKE's native GPU pool behaviour. See [GPU Nodes](../gpu-nodes.md) for details.

```yaml
apiVersion: karpenter.k8s.gcp/v1alpha1
kind: GCENodeClass
metadata:
  name: gpu
spec:
  autoGPUTaint: true
  imageSelectorTerms:
    - alias: ContainerOptimizedOS@latest
  metadata:
    kube-labels: "cloud.google.com/gke-gpu-driver-version=latest"
  disks:
    - category: pd-balanced
      sizeGiB: 100
      boot: true
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: gpu
spec:
  template:
    spec:
      nodeClassRef:
        group: karpenter.k8s.gcp
        kind: GCENodeClass
        name: gpu
      requirements:
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["on-demand"]
        - key: karpenter.k8s.gcp/instance-gpu-count
          operator: Exists
        - key: kubernetes.io/arch
          operator: In
          values: ["amd64"]
  disruption:
    consolidationPolicy: WhenEmpty
    consolidateAfter: 5m
```

GPU workload:

See [`examples/workload/gpu-workload.yaml`](https://github.com/cloudpilot-ai/karpenter-provider-gcp/blob/main/examples/workload/gpu-workload.yaml).

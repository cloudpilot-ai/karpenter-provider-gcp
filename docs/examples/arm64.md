# arm64 nodes (Google Axion / Ampere Altra)

Use `c4a` (Google Axion, based on Arm Neoverse V2) or `t2a` (Ampere Altra) instance families for arm64 workloads.

```yaml
apiVersion: karpenter.k8s.gcp/v1alpha1
kind: GCENodeClass
metadata:
  name: arm64
spec:
  imageSelectorTerms:
    - alias: ContainerOptimizedOS@latest
  disks:
    - category: pd-balanced
      sizeGiB: 60
      boot: true
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: arm64
spec:
  template:
    spec:
      nodeClassRef:
        group: karpenter.k8s.gcp
        kind: GCENodeClass
        name: arm64
      requirements:
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["spot", "on-demand"]
        - key: karpenter.k8s.gcp/instance-family
          operator: In
          values: ["c4a", "t2a"]
        - key: kubernetes.io/arch
          operator: In
          values: ["arm64"]
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    consolidateAfter: 0s
```

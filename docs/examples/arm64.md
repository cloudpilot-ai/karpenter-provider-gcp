# arm64 nodes (Google Axion / Ampere Altra)

Karpenter automatically detects arm64 architecture from the GCP Compute API. All arm64-capable instance families work without explicit configuration, including `c4a` (Google Axion, Arm Neoverse V2), `t2a` (Ampere Altra), and `n4a` (Google Axion). New arm64 families are supported automatically as GCP releases them.

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
          values: ["c4a", "t2a", "n4a"]
        - key: kubernetes.io/arch
          operator: In
          values: ["arm64"]
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    consolidateAfter: 0s
```

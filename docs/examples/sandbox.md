# Sandbox workloads

This minimal non-GitOps example uses the native GKE `gvisor` RuntimeClass; it does not create or overwrite it. Verify its handler, scheduling selector and toleration first. See [Sandbox nodes](../sandbox-nodes.md) for IAM, COS/bootstrap compatibility, GitOps ownership and live-acceptance limitations. Keep the ordinary system pool; no native sandbox pool is required.

The NodePool controller adds the runtime template label and permanent `NoSchedule` taint automatically. Do not duplicate them for this workflow. For a new GitOps-managed pool, instead explicitly declare both as described in [GitOps ownership](../sandbox-nodes.md#gitops-ownership).

```yaml
apiVersion: karpenter.k8s.gcp/v1alpha1
kind: GCENodeClass
metadata:
  name: sandbox
spec:
  sandboxConfig:
    type: GVISOR
  imageSelectorTerms:
    - family: ContainerOptimizedOS
      channel: cluster
  disks:
    - boot: true
      sizeGiB: 60
      category: pd-balanced
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: sandbox
spec:
  template:
    spec:
      nodeClassRef:
        group: karpenter.k8s.gcp
        kind: GCENodeClass
        name: sandbox
      requirements:
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["on-demand"]
        - key: karpenter.k8s.gcp/instance-family
          operator: In
          values: ["n2", "n2d"]
        - key: kubernetes.io/arch
          operator: In
          values: ["amd64"]
  disruption:
    consolidationPolicy: WhenEmpty
    consolidateAfter: 5m
---
apiVersion: v1
kind: Pod
metadata:
  name: sandbox-example
spec:
  runtimeClassName: gvisor
  containers:
    - name: pause
      image: registry.k8s.io/pause:3.10
      resources:
        requests:
          cpu: 100m
          memory: 64Mi
```

Kubernetes RuntimeClass admission supplies the Pod's runtime selector and toleration (and any administrator-configured overhead); no duplicate Pod selector, toleration or sandbox overhead is needed here. Administrators can use a custom RuntimeClass name targeting handler `gvisor` with the same safe scheduling contract. This example is not a boot/isolation test and does not demonstrate GPU/CUDA acceptance.

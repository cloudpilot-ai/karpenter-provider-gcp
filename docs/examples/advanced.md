# Advanced examples

## Instance metadata

Set GCE instance metadata key-value pairs, for example to pin the GPU driver version:

See [`examples/nodeclass/gcenodeclass-metadata.yaml`](https://github.com/cloudpilot-ai/karpenter-provider-gcp/blob/main/examples/nodeclass/gcenodeclass-metadata.yaml).

## Secondary boot disk (container image pre-loading)

Pre-load container images onto a secondary boot disk to reduce node startup latency. See [GKE container image pre-loading](https://cloud.google.com/kubernetes-engine/docs/how-to/data-container-image-preloading) for how to build the pre-loaded image.

See [`examples/nodeclass/secondary-disk-gcenodeclass.yaml`](https://github.com/cloudpilot-ai/karpenter-provider-gcp/blob/main/examples/nodeclass/secondary-disk-gcenodeclass.yaml).

## Custom kubelet settings

Override pod density and resource reservations:

```yaml
kubeletConfiguration:
  maxPods: 50
  systemReserved:
    cpu: 100m
    memory: 100Mi
  kubeReserved:
    cpu: 100m
    memory: 200Mi
  evictionHard:
    memory.available: 200Mi
    nodefs.available: 10%
```

## Shielded VM

Shielded VM provides verifiable integrity for your instances, protecting against boot-level and kernel-level malware. GCP organizations that enforce `constraints/compute.requireShieldedVm` require these settings on all instances. Without them, Karpenter-provisioned nodes fail with a `412 conditionNotMet` error.

```yaml
shieldedInstanceConfig:
  enableSecureBoot: true
  enableVtpm: true
  enableIntegrityMonitoring: true
```

The options are:

- **enableSecureBoot**: Verifies all boot components (firmware, bootloader, kernel) are signed by trusted publishers. Prevents boot-level rootkits.
- **enableVtpm**: Enables a virtual Trusted Platform Module that validates guest VM integrity before and during boot.
- **enableIntegrityMonitoring**: Monitors and reports changes to the boot sequence. View integrity reports in Cloud Monitoring.

See [GCP Shielded VM documentation](https://cloud.google.com/compute/shielded-vm/docs/shielded-vm) for details on each feature.

## Multiple NodePools

Run independent pools for different workload tiers, each referencing a different GCENodeClass:

```yaml
# Spot pool for batch workloads
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: batch
spec:
  template:
    metadata:
      labels:
        tier: batch
    spec:
      nodeClassRef:
        group: karpenter.k8s.gcp
        kind: GCENodeClass
        name: default-example
      requirements:
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["spot"]
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    consolidateAfter: 0s
---
# On-demand pool for system workloads
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: system
spec:
  weight: 100   # higher weight = preferred
  template:
    metadata:
      labels:
        tier: system
    spec:
      nodeClassRef:
        group: karpenter.k8s.gcp
        kind: GCENodeClass
        name: default-example
      requirements:
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["on-demand"]
        - key: karpenter.k8s.gcp/instance-family
          operator: In
          values: ["n2"]
  disruption:
    consolidationPolicy: WhenEmpty
    consolidateAfter: 30m
```

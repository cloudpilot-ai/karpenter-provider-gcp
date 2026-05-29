# Advanced examples

## Instance metadata

Set GCE instance metadata key-value pairs for advanced configuration. Values in `spec.metadata` override matching keys from the base instance template; new keys are added.

See [`examples/nodeclass/gcenodeclass-metadata.yaml`](https://github.com/cloudpilot-ai/karpenter-provider-gcp/blob/main/examples/nodeclass/gcenodeclass-metadata.yaml).

For example, if the base GKE node-pool template sets `serial-port-logging-enable=true`, specifying `serial-port-logging-enable: "false"` in `spec.metadata` results in the provisioned instance having `serial-port-logging-enable=false`.

> **Note:** For GPU driver version control, use `spec.gpuDriverVersion` instead of setting `cloud.google.com/gke-gpu-driver-version` via metadata. See [GPU Nodes](../gpu-nodes.md) for details.

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

## Confidential VM

Confidential VM provides in-use memory encryption via AMD SEV / SEV-SNP or Intel TDX. It is only supported on specific machine families; see the [GCP support matrix](https://cloud.google.com/confidential-computing/confidential-vm/docs/os-and-machine-type) for the current list.

```yaml
confidentialInstanceConfig:
  enableConfidentialCompute: true
  confidentialInstanceType: SEV_SNP  # SEV | SEV_SNP | TDX
```

The options are:

- **enableConfidentialCompute**: Enables in-use memory encryption on the instance. Karpenter forces `scheduling.onHostMaintenance` to `TERMINATE` on provisioned instances because Confidential VMs cannot live-migrate.
- **confidentialInstanceType**: Selects the confidential compute technology (`SEV`, `SEV_SNP`, or `TDX`). Optional — when omitted with `enableConfidentialCompute: true`, GCE picks the default for the selected machine family.

If the chosen machine type does not support the requested confidential type, GCE rejects the instance creation and the error surfaces on the `NodeClaim` `Launched` condition.

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

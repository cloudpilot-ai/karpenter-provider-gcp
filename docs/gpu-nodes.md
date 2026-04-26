# GPU Nodes

Karpenter can provision GPU-equipped GKE nodes for workloads that request `nvidia.com/gpu` resources.

## Instance types

**Built-in GPU families** — `a2`, `a3`, `g2`, `a4`. These machine families have NVIDIA accelerators integrated into the machine type. Select them via `karpenter.k8s.gcp/instance-gpu-count`.

> **Note:** Attached GPU instances (e.g. `n1-standard-*` + NVIDIA T4/P100/V100 accelerators) are not yet fully supported. Tracked in [#84](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/84).

## Device plugin scheduling

Karpenter automatically injects the `cloud.google.com/gke-accelerator=<type>` label into the node's `kube-labels` metadata for all GPU instances. This label is required by the NVIDIA device plugin DaemonSet's `nodeAffinity`, so without it the plugin would not schedule onto Karpenter-provisioned GPU nodes and `nvidia.com/gpu` would never become allocatable.

No additional configuration is needed — the label is derived from the instance type's accelerator type and injected at provisioning time.

## Driver installation

Karpenter automatically installs the GKE-recommended stable NVIDIA driver on GPU nodes. Set `gpuDriverVersion` to choose between the stable and latest drivers:

```yaml
spec:
  gpuDriverVersion: latest
```

| Value     | Terraform equivalent | Behaviour                                                             |
|-----------|----------------------|-----------------------------------------------------------------------|
| `default` | `DEFAULT`            | GKE-recommended stable driver. Works on COS and Ubuntu. (**default**) |
| `latest`  | `LATEST`             | Newest available driver. COS only.                                    |

Karpenter injects `cloud.google.com/gke-gpu-driver-version=<value>` as a node label at provisioning time. GKE's GPU driver installer DaemonSet reads this label to determine which driver to install. The label is only set on GPU instances — non-GPU instances are unaffected.

## Auto GPU taint

GKE natively taints GPU nodes with `nvidia.com/gpu=present:NoSchedule` so that only GPU-tolerating workloads are scheduled on them. Karpenter does not apply this taint by default.

Enable `autoGPUTaint` on a `GCENodeClass` to replicate GKE's behaviour:

```yaml
apiVersion: karpenter.k8s.gcp/v1alpha1
kind: GCENodeClass
metadata:
  name: gpu
spec:
  autoGPUTaint: true
  gpuDriverVersion: latest
  imageSelectorTerms:
    - alias: ContainerOptimizedOS@latest
```

When `autoGPUTaint: true`, Karpenter injects `--register-with-taints=nvidia.com/gpu=present:NoSchedule` into the node's `KUBELET_ARGS` at provisioning time. The node registers with the taint, preventing non-GPU workloads from landing on it.

The field defaults to `false` for backward compatibility. Existing deployments are unaffected unless they opt in.

### Workload toleration

GPU workloads must tolerate the taint:

```yaml
tolerations:
  - key: nvidia.com/gpu
    operator: Exists
    effect: NoSchedule
```

## Example

See [`docs/examples/gpu.md`](examples/gpu.md) for a complete `GCENodeClass` + `NodePool` configuration.

# GPU Nodes

Karpenter can provision GPU-equipped GKE nodes for workloads that request `nvidia.com/gpu` resources.

## Instance types

Two categories of GPU instances are supported:

- **Built-in GPU families** — `a2`, `a3`, `g2`, `a4`. These machine families have NVIDIA accelerators integrated into the machine type. Select them via `karpenter.k8s.gcp/instance-gpu-count`.
- **Attached GPU instances** — standard instance types (e.g. `n1`) with NVIDIA accelerators attached via the GKE node pool template. The template carries the `GuestAccelerators` configuration.

## Driver installation

Set the `cloud.google.com/gke-gpu-driver-version` label in `spec.metadata` to have GKE install the GPU driver automatically:

```yaml
spec:
  metadata:
    kube-labels: "cloud.google.com/gke-gpu-driver-version=latest"
```

Supported values: `latest`, `default`, or a specific driver version string.

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
  imageSelectorTerms:
    - alias: ContainerOptimizedOS@latest
  metadata:
    kube-labels: "cloud.google.com/gke-gpu-driver-version=latest"
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

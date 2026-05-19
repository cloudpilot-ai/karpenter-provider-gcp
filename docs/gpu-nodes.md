# GPU Nodes

Karpenter can provision GPU-equipped GKE nodes for workloads that request `nvidia.com/gpu` resources.

## Instance types

**Built-in GPU families** — `a2`, `a3`, `g2`, `a4`. These machine families have NVIDIA accelerators integrated into the machine type. Select them via `karpenter.k8s.gcp/instance-gpu-count`.

> **Note:** Attached GPU instances (e.g. `n1-standard-*` + NVIDIA T4/P100/V100 accelerators) are not yet fully supported. Tracked in [#84](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/84).

## GKE GPU software stack labels

GKE's GPU software stack requires two labels for GPU nodes to function:

- `cloud.google.com/gke-accelerator=<type>` — used by the NVIDIA device plugin DaemonSet to target GPU nodes via node affinity.
- `cloud.google.com/gke-gpu-driver-version=<value>` — used by the GKE GPU driver installer to select the NVIDIA driver version.

Karpenter automatically applies both labels at node boot time. The accelerator type is derived from the instance type. The driver version is controlled by `spec.gpuDriverVersion`.

## GPU driver version

The `gpuDriverVersion` field on `GCENodeClass` controls which NVIDIA driver GKE installs on GPU nodes. Valid values:

| Value      | Description                             |
|------------|-----------------------------------------|
| `default`  | GKE-recommended stable driver (default) |
| `latest`   | Newest available driver (COS only)      |
| `disabled` | Skip automatic driver installation      |

```yaml
apiVersion: karpenter.k8s.gcp/v1alpha1
kind: GCENodeClass
metadata:
  name: gpu
spec:
  gpuDriverVersion: default  # or: latest, disabled
  imageSelectorTerms:
    - alias: ContainerOptimizedOS@latest
```

Defaults to `"default"`, matching GKE's native behavior for GPU node pools.

> **Note:** NodePool `spec.template.spec.labels` does not affect the driver version at boot time. NodePool labels are applied post-registration, after the GPU driver installer has already run. Use `GCENodeClass.spec.gpuDriverVersion` to control which driver GKE installs.

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
  gpuDriverVersion: default
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

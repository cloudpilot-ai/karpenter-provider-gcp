# GPU Nodes

Karpenter can provision GPU-equipped GKE nodes for workloads that request `nvidia.com/gpu` resources.

## Instance types

**Built-in GPU families** — `a2`, `a3`, `g2`, `a4`. These machine families have NVIDIA accelerators integrated into the machine type. Select them via `karpenter.k8s.gcp/instance-gpu-count`.

The `karpenter.k8s.gcp/instance-gpu-count` label and the `nvidia.com/gpu` capacity of each instance type report the number of GPUs on the machine type. For example, `g2-standard-96` reports `8`. Use `operator: Exists` to allow any GPU machine type, or `operator: In` to require an exact GPU count:

```yaml
- key: karpenter.k8s.gcp/instance-gpu-count
  operator: In
  values: ["1"]
```

> **Note:** Earlier releases reported a count of `1` for every built-in GPU machine type. After upgrading, a requirement such as `instance-gpu-count In ["1"]` matches only single-GPU machine types, so Karpenter no longer launches multi-GPU machines such as `g2-standard-96` for it.

> **Note:** Attached GPU instances (e.g. `n1-standard-*` + NVIDIA T4/P100/V100 accelerators) are not yet fully supported. Tracked in [#84](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/84).

## GKE GPU software stack labels

GKE's GPU software stack requires several labels for GPU nodes to function:

- `cloud.google.com/gke-accelerator=<type>` — used by the NVIDIA device plugin DaemonSet to target GPU nodes via node affinity.
- `cloud.google.com/gke-gpu-driver-version=<value>` — used by the GKE GPU driver installer to select the NVIDIA driver version.
- `cloud.google.com/gke-cpu-scaling-level=<cpu>` — used by GKE's NVIDIA device plugin DaemonSets to select the appropriate plugin variant.

Karpenter applies these labels at node boot time. The accelerator type and CPU scaling level are derived from the instance type. The driver version is controlled by `spec.gpuDriverVersion`.

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
    - family: ContainerOptimizedOS
      channel: cluster
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
    - family: ContainerOptimizedOS
      channel: cluster
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

## GPU time-sharing

GKE can let several containers share one physical GPU by time-slicing it. Set `gpuSharingConfig` on a `GCENodeClass` to provision nodes the same way a GKE node pool with `gpu_sharing_config` does:

```yaml
apiVersion: karpenter.k8s.gcp/v1alpha1
kind: GCENodeClass
metadata:
  name: gpu-shared
spec:
  autoGPUTaint: true
  gpuSharingConfig:
    strategy: TimeSharing
    maxSharedClientsPerGPU: 4
  imageSelectorTerms:
    - family: ContainerOptimizedOS
      channel: cluster
```

On GPU instance types from this NodeClass, Karpenter:

- writes `GPU_SHARING_STRATEGY`, `MAX_SHARED_CLIENTS_PER_GPU` and `MAX_TIME_SHARED_CLIENTS_PER_GPU` into kube-env, which the GKE NVIDIA device plugin reads;
- labels the node with `cloud.google.com/gke-gpu-sharing-strategy=time-sharing` and `cloud.google.com/gke-max-shared-clients-per-gpu=<n>`;
- schedules against `nvidia.com/gpu` capacity of GPU count × `maxSharedClientsPerGPU`. A `g2-standard-24` (2 × L4) with `maxSharedClientsPerGPU: 4` offers `nvidia.com/gpu: 8`.

Workloads opt in with the same node selectors they use on GKE node pools, and request `nvidia.com/gpu: 1`:

```yaml
nodeSelector:
  cloud.google.com/gke-gpu-sharing-strategy: time-sharing
  cloud.google.com/gke-max-shared-clients-per-gpu: "4"
```

Only `TimeSharing` is supported. Time-sharing gives no memory or fault isolation between containers on the same GPU. The field is ignored for non-GPU instance types.

## GKE Sandbox GPUs

Set `spec.sandboxConfig.type: GVISOR` to opt in to [sandbox nodes](sandbox-nodes.md). Sandbox GPU capacity requires real public COS sandbox GPU images (including Arm64 where published), compatible built-in GPU models, whole-GPU capacity and `default`/`latest` drivers. `disabled` drivers, GPU time-sharing, fractional GPUs and V100/P100 are unsupported. This does not add attached-GPU support.

Both the selected image and effective bootstrap/kubelet release must meet the [sandbox GPU model/version floors](sandbox-nodes.md#gpu-compatibility), and the complete bootstrap consumers must match a reviewed compatibility profile. A GKE version floor alone is not sufficient. Pods use a safely configured `runtimeClassName: gvisor`; GPU driver/device-plugin DaemonSets must tolerate the permanent sandbox taint as well as any GPU taint. No live CUDA acceptance has yet been performed for reconstructed sandbox nodes.

## Example

See [`docs/examples/gpu.md`](examples/gpu.md) for a complete `GCENodeClass` + `NodePool` configuration.

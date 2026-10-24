# Sandbox nodes (gVisor)

GKE Sandbox runs Pods with gVisor rather than the ordinary container runtime. Enable it on a `GCENodeClass`, reference that class from a NodePool, and select a gVisor RuntimeClass in your Pods. See the [complete example](examples/sandbox.md).

```yaml
spec:
  sandboxConfig:
    type: GVISOR
  imageSelectorTerms:
    - family: ContainerOptimizedOS
      channel: cluster
```

Omitting `sandboxConfig` provisions ordinary nodes. `GVISOR` is the only supported type; [MicroVM support](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/707) is separate.

> **Validation status:** reconstructed-node boot, workload isolation and CUDA execution have not yet been live-validated. The [E2E smoke tests](../test/suites/sandbox/README.md) are not a security certification. Review [GKE Sandbox workload limitations](https://docs.cloud.google.com/kubernetes-engine/docs/concepts/sandbox-pods) before enabling it.

## Prerequisites

- Use GKE Standard and retain an ordinary system pool for Karpenter and essential workloads. **No native sandbox pool is required**; ordinary [bootstrap pool discovery](bootstrap-pool.md) still applies.
- Upgrade the controller and [CRDs](getting-started/upgrading.md) together before setting the new field.
- Give the controller **`compute.disks.get`** for boot-image/adoption/drift checks. It is declared in the [controller IAM role](../deploy/iam/karpenter-controller-role.yaml), also consumed by the [Terraform module](../deploy/terraform/); existing custom-role installations need their normal IAM update.
- Allow controller access to Compute/GKE APIs, `gke.gcr.io` and the public `gke-release`, `gke-release-asia` and `gke-release-eu` buckets at `storage.googleapis.com`. Nodes need their normal bootstrap access plus the gVisor installer at `gke.gcr.io`.
- Provide a GKE- or administrator-managed RuntimeClass with the scheduling settings below. Karpenter does not create or modify RuntimeClasses.
- **Check bootstrap compatibility before GKE upgrades.** Unknown bootstrap consumers block new sandbox provisioning, even when native GKE Sandbox supports that version. The initial reviewed profile is `1.35.6-gke.1250001`; changed consumers require a reviewed controller update, with no bypass. Compatibility uses the effective bootstrap/kubelet release, not just the control-plane version.

## Run sandboxed Pods

Inspect the existing class with `kubectl get runtimeclass gvisor -o yaml`. It must target handler `gvisor` and supply the runtime selector and toleration:

```yaml
handler: gvisor
scheduling:
  nodeSelector:
    sandbox.gke.io/runtime: gvisor
  tolerations:
    - key: sandbox.gke.io/runtime
      operator: Equal
      value: gvisor
      effect: NoSchedule
```

This is a settings excerpt, not an instruction to overwrite a GKE-managed class. A custom class name is supported; use its name in `spec.runtimeClassName` on the Pod. Admission supplies the selector, toleration and any configured overhead; do not duplicate them. A handler alone, without scheduling constraints, is unsafe because the Pod could land on an ordinary node.

The controller automatically adds the `sandbox.gke.io/runtime=gvisor` NodePool template label and permanent `sandbox.gke.io/runtime=gvisor:NoSchedule` taint, matching [native GKE defaults](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/sandbox-pods#run_a_regular_pod_along_with_sandboxed_pods). Do not use a startup taint. Conflicting labels, taints or requirements must be corrected in the manifest; the controller preserves user intent rather than overwriting it.

Ordinary Pods without the toleration cannot use sandbox capacity. Explicitly tolerating the taint permits ordinary Pods onto those nodes but **does not select gVisor**; that requires the RuntimeClass.

### GitOps ownership

For a **new GitOps-managed NodePool**, declare both runtime fields before reconciliation:

```yaml
spec:
  template:
    metadata:
      labels:
        sandbox.gke.io/runtime: gvisor
    spec:
      taints:
        - key: sandbox.gke.io/runtime
          value: gvisor
          effect: NoSchedule
```

These fields remain user-owned. Remove both from Git, along with runtime requirements, when disabling sandboxing or switching to an ordinary NodeClass. Otherwise they are preserved and block ordinary scheduling.

Adding already controller-managed values to Git does not transfer ownership. Existing auto-managed pools must allow the narrow runtime fields and top-level `karpenter.k8s.gcp/sandbox-ownership` annotation to coexist with reconciliation, or migrate to a new declarative pool. Do not ignore all labels/taints or edit/copy the ownership annotation. The controller removes only its still-owned canonical values; unrelated fields and user edits survive.

## Images

Use supported public GKE **COS** images from `gke-node-images`. Ubuntu, mixed selectors, contradictory `imageFamily` overrides and custom/copied images are unsupported. Image IDs, channels and COS pins are honored; a CPU image ID does not also select a GPU image. Missing compatible images/installers fail resolution rather than silently substituting another build. See [image selection](image-selection.md).

CPU nodes support amd64 and Arm64. GPU nodes require a published sandbox GPU image for the machine's architecture; CPU and GPU COS builds can differ, so an exact COS pin may exclude GPU capacity. The provider discovers variants and selects an immutable installer for each image; do not override reserved runtime metadata or labels.

## GPU compatibility

Sandbox GPUs use the provider's compatible built-in, whole-GPU machine catalog. V100/P100, fractional GPUs, GPU time-sharing, attached GPUs and TPUs are unsupported. Use `gpuDriverVersion: default` (the default) or `latest`, not `disabled`.

Both the selected image and effective bootstrap/kubelet releases must meet these GPU floors:

| GPU models                                            | Minimum GKE release                                                                                                 |
|-------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------|
| T4, L4, A100 (including 80 GB), H100 (including Mega) | `1.29.15-gke.1134000`; on 1.30: `1.30.11-gke.1093000`; on 1.31: `1.31.7-gke.1149000`; on 1.32: `1.32.2-gke.1182003` |
| H200, B200, GB200                                     | `1.34.0-gke.1713000`                                                                                                |
| RTX PRO 6000                                          | `1.34.1-gke.2037001`                                                                                                |

Later minors still require reviewed bootstrap compatibility and a published compatible image; this table does not extend the provider's machine catalog. Follow the [GPU driver/device-plugin requirements](gpu-nodes.md). Required system DaemonSets must tolerate the sandbox taint and any additional GPU or workload-pool taints.

## Upgrades and disabling

Configuration, image, installer or bootstrap-policy changes can cause drift. Image pins do not freeze bootstrap compatibility. Normal NodePool disruption controls and workload disruption budgets govern replacement; existing VMs are **not converted in place**.

To disable, update workload templates to remove the gVisor RuntimeClass or target another sandbox pool, remove `sandboxConfig`, and remove user-owned runtime labels/taints/requirements. The controller cleans up its own fields. Retain the ordinary system pool. Before downgrading to a controller without sandbox support, migrate/retire sandbox workloads and capacity first.

## Troubleshooting

Start with these read-only checks:

```bash
kubectl get gcenodeclass sandbox -o jsonpath='{.status.conditions}'
kubectl get gcenodeclass sandbox -o jsonpath='{.status.images}'
kubectl get nodepool sandbox -o jsonpath='{.spec.template}'
kubectl get runtimeclass gvisor -o yaml
kubectl describe pod sandbox-example
```

| Symptom                                                      | Action                                                                                                                                                      |
|--------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `ImagesReady=False` / `ImageResolutionFailed`                | Check selectors, public COS provenance, exact GPU variants/pins, installer availability and artifact egress.                                                |
| `SandboxReady=False` / `SandboxBootstrapUnsupported`         | Read the condition message. Retry transient network errors; unknown bootstrap consumers need a reviewed controller update, not patched status/fingerprints. |
| `SandboxConfigurationUnsupported` / no compatible candidates | Check GPU model, image/bootstrap versions, driver mode and unsupported sharing or metadata overrides.                                                       |
| Pod admission or scheduling failure                          | Check the RuntimeClass, admitted selector/toleration, persisted NodePool runtime fields, conflicting requirements and GitOps reconciliation.                |
| Adoption/drift comparison error                              | Check `compute.disks.get` and actual boot-image availability; do not relabel ordinary VMs or forge runtime identity.                                        |

`SandboxReady=True` reports bootstrap capability, not workload isolation or successful node/CUDA execution. When sharing diagnostics, use condition messages and public artifact identities; never include raw `kube-env`, source templates, instance metadata, tokens, certificates or credentials.

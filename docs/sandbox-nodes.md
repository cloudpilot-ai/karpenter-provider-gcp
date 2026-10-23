# Sandbox nodes (gVisor)

GKE Sandbox runs Pods with gVisor rather than the ordinary container runtime. Opt in on a `GCENodeClass`:

```yaml
spec:
  sandboxConfig:
    type: GVISOR
  imageSelectorTerms:
    - family: ContainerOptimizedOS
      channel: cluster
```

Omitting `sandboxConfig` continues to provision ordinary nodes. `GVISOR` is the only supported type; there is no enable boolean or installer-version setting. MicroVM support (GKE 1.37+) is deferred to [#707](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/707).

## Prerequisites and support boundary

- Use GKE Standard with an ordinary system pool for Karpenter and essential cluster workloads. **Creating a native sandbox pool is not a prerequisite.** Existing [bootstrap pool discovery](bootstrap-pool.md) still supplies cluster-specific bootstrap material.
- Upgrade the controller and [CRDs](getting-started/upgrading.md) together before setting the new field.
- Existing minimal controller IAM custom roles need **`compute.disks.get`**, now declared in [`deploy/iam/karpenter-controller-role.yaml`](../deploy/iam/karpenter-controller-role.yaml). It reads the actual boot disk's source image for sandbox adoption/drift comparisons. The [Terraform module](../deploy/terraform/) reads this role file; existing installations still need their normal reviewed IAM update. Ordinary provisioning does not acquire a sandbox disk/profile prerequisite.
- Permit controller access to the Compute/GKE APIs, `gke.gcr.io` and public GKE artifact mirrors under `storage.googleapis.com/gke-release`, `gke-release-asia` and `gke-release-eu`. Nodes also need their usual GKE bootstrap artifact access and access to the public gVisor installer at `gke.gcr.io`. Check DNS, TLS, proxy and egress policies.
- Verify a GKE- or administrator-managed RuntimeClass with safe scheduling settings, as described below. Karpenter neither creates nor modifies RuntimeClasses and adds no RuntimeClass RBAC.
- Check the effective bootstrap compatibility profile **before GKE upgrades**. Unknown consumer schemas block **new sandbox provisioning**, even if the new GKE version supports native sandbox pools. A reviewed controller compatibility-profile update may be required; there is no bypass switch.

The initial reviewed public-consumer fixture and seven zero-node native-template comparisons used **GKE `1.35.6-gke.1250001`**. This is not proof of every GKE release. No reconstructed-node boot, runtime isolation or CUDA execution has yet been validated. Separately authorized live acceptance is still required, including amd64/Arm64, supported GPUs, networking, storage and system DaemonSets. Review [GKE Sandbox limitations](https://docs.cloud.google.com/kubernetes-engine/docs/concepts/sandbox-pods) for workload compatibility.

## Images and runtime artifacts

Sandboxing is **COS-only**. Admission checks every image selector and any `imageFamily` override: Ubuntu, Ubuntu aliases, mixed COS/Ubuntu selectors and contradictory overrides are rejected. Explicit IDs must resolve to actual READY, non-deprecated, supported public GKE COS images in `gke-node-images`, with the correct architecture and CPU/sandbox-GPU role. A COS override, copied image or convincing custom image name is not provenance.

CPU nodes use ordinary public COS images across the provider's existing amd64/Arm64 CPU and custom-machine catalog, subject to image/bootstrap compatibility. GPU nodes require real published sandbox GPU variants (`-gv-nvda`, or older `-gvisor-nvda`), including Arm64 where available. Karpenter discovers these variants; it does not fabricate names or fall back to ordinary GPU images.

Explicit image IDs, release-channel builds and COS version pins are honored. A CPU image ID does not implicitly select a GPU variant. CPU and sandbox GPU images can have different COS builds within the same GKE build; a COS version pin restricts both to that COS version. If the requested compatible variant is absent, that capacity is unavailable rather than silently substituting another build. See [Image selection](image-selection.md).

Each selected image's public `gvisor-version` label resolves to a content-verified, immutable installer digest from `gke.gcr.io/gke-gvisor-installer`. `status.images[].sandboxInstaller` records its version and digest. Missing/unknown labels, unavailable installer tags, invalid digests or unsupported platforms fail resolution; Karpenter does not guess a version or use `latest` as an installer fallback. Image and artifact caches refresh successful resolutions within five minutes.

### Strict bootstrap compatibility

The effective bootstrap source, not just the cluster master version, determines compatibility. Its `KUBE_MANIFESTS_TAR_URL` and `SERVER_BINARY_TAR_URL` must identify the same exact public GKE release. The controller downloads only validated fixed-host public manifests and compares the **complete SHA256 hashes** of:

- `kubernetes/gci-trusty/gci-configure-helper.sh`
- `kubernetes/gci-trusty/gke-internal-configure-helper.sh`
- `kubernetes/gci-trusty/installable.py`

All consumers must match the accepted fixture-backed schema. Unknown, missing or changed consumers fail closed with `SandboxBootstrapUnsupported`; semver support or matching text markers is not sufficient. Downloads are bounded (2 MiB compressed, 16 MiB decompressed total, 2 MiB per member); scripts are not executed or extracted to disk.

Matching helpers proves consumer compatibility, **not discovery of GKE's complete native hardware/security policy**. The controller maintains that policy separately: `xemu`, SMT enabled, nftables reproduction, and core tags for Intel/mixed-vendor amd64 families; documented AMD-only families and Arm omit core tags. Custom CPU machine types use their family policy. SMT does not halve advertised vCPU capacity. Accepting changed consumers requires reviewing consumers and native policy plus regression/live evidence, not simply adding a version to a list.

## Pod scheduling and RuntimeClass

Pods opt in with **`runtimeClassName: gvisor`**. Use the native GKE `gvisor` RuntimeClass when present. Inspect it rather than assuming it exists on every cluster:

```bash
kubectl get runtimeclass gvisor -o yaml
```

Expected settings are:

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

This is a settings excerpt, not an instruction to overwrite the GKE-managed object. Administrators may own custom RuntimeClass names using handler `gvisor` and the same selector/toleration; use that name in the Pod. Karpenter has no dependency on the literal class name. Kubernetes admission supplies scheduling fields and any configured Pod overhead; upstream Karpenter already accounts for that overhead. Do not add a second sandbox overhead reservation.

Native GKE itself adds the matching `sandbox.gke.io/runtime=gvisor` label and `sandbox.gke.io/runtime=gvisor:NoSchedule` taint to sandbox nodes by default. See [Run a regular Pod along with sandboxed Pods](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/sandbox-pods#run_a_regular_pod_along_with_sandboxed_pods). To reproduce that native scheduling contract, for each NodePool referencing a sandbox NodeClass the `nodepool.sandbox` controller adds only:

- `spec.template.metadata.labels["sandbox.gke.io/runtime"]: gvisor`
- The **permanent** `spec.template.spec.taints` entry `sandbox.gke.io/runtime=gvisor:NoSchedule` (not a startup taint).
- A top-level `metadata.annotations["karpenter.k8s.gcp/sandbox-ownership"]` recording independently whether it added the label and the taint.

It preserves preexisting identical user fields, conflicting values and unrelated labels/taints. Missing or conflicting **persisted** fields, runtime startup taints and contradictory requirements gate scheduling; scheduling does not repair its input NodePool in memory. Resolve conflicts in the manifest instead of expecting them to be overwritten.

Ordinary capacity advertises absence of the runtime label; sandbox capacity advertises `gvisor`. An ordinary Pod without the toleration cannot trigger sandbox provisioning. A safely configured gVisor RuntimeClass Pod cannot receive ordinary capacity. Regular Pods that explicitly tolerate the sandbox taint may use sandbox nodes, matching native GKE behavior, but toleration alone does not select the gVisor runtime; that requires a RuntimeClass targeting handler `gvisor`. A RuntimeClass with only a handler, but no runtime selector, is unsafe: it can allow ordinary placement and later fail at runtime. `SandboxReady` does not validate cluster-wide RuntimeClass policy.

See the [complete minimal example](examples/sandbox.md); it deliberately omits the auto-managed NodePool fields.

### GitOps ownership

For **new GitOps-managed NodePools**, explicitly declare both canonical fields in the desired manifest before the controller observes it:

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

These preexisting fields remain user-owned, so the controller does not need to add or track them. Keep unrelated fields under normal GitOps management. When disabling sandboxing or changing the NodeClass reference to an ordinary class, **remove both user-owned fields from Git**, together with any runtime requirements. Otherwise they are intentionally preserved and block ordinary scheduling.

Declaring an already controller-added value in Git does not transfer its recorded ownership. Existing auto-managed pools must allow the controller's narrow runtime fields and its top-level ownership annotation to coexist with GitOps reconciliation, or migrate deliberately to a new declaratively configured pool. Do not ignore all labels or taints, and do not forge, copy or edit the ownership annotation. On disable/reference changes the controller removes only still-owned canonical values, independently for the label and taint; user edits and unrelated fields survive.

## GPU compatibility

The provider's compatible built-in GPU catalog is supported, subject to whole-GPU capacity, model, image, bootstrap and driver checks. Support is not restricted to the zero-node template samples. This does not add attached GPUs, TPU support, fractional GPUs or GPU time-sharing. V100/P100 are unsupported. Only `gpuDriverVersion: default` (also the default when omitted) or `latest` is supported for sandbox GPUs; `disabled` is not.

Both the **selected image release and effective bootstrap/kubelet release** must meet these implemented GKE GPU floors:

| GPU models                                            | Minimum GKE release                                                                                                 |
|-------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------|
| T4, L4, A100 (including 80 GB), H100 (including Mega) | `1.29.15-gke.1134000`; on 1.30: `1.30.11-gke.1093000`; on 1.31: `1.31.7-gke.1149000`; on 1.32: `1.32.2-gke.1182003` |
| H200, B200, GB200                                     | `1.34.0-gke.1713000`                                                                                                |
| RTX PRO 6000                                          | `1.34.1-gke.2037001`                                                                                                |

Later minors pass the model floor but **still require the strict bootstrap-schema match and a published compatible image/installer**. A model in this table does not add it to the provider's machine catalog or prove its live CUDA behavior. Arm64 GPU candidates require a real published Arm64 sandbox GPU image and compatible built-in machine type. Keep the existing [GPU software stack and device plugin requirements](gpu-nodes.md), including tolerations for any additional GPU taint. Driver/device-plugin/system DaemonSets must also tolerate the sandbox taint where needed.

## Identity, drift and disabling

New sandbox VMs carry provider-owned runtime, immutable installer and bootstrap schema/policy/hardware-profile identity, plus the launch NodeClass UID/hash and NodePool owner UID when supplied by a genuine owner reference. Full identity is in VM metadata (not a digest truncated into a GCE label), and is surfaced on NodeClaims. Do not override these reserved metadata/label keys.

Get/List/adoption use actual VM facts, not today's NodeClass or an instance-type cache. Ordinary or stale-runtime VMs cannot be adopted as sandbox capacity; malformed/partial identity fails closed. Historical VMs remain discoverable for normal lifecycle/garbage collection even when their configuration is no longer launchable.

Enabling/disabling sandboxing changes the NodeClass hash; changed images use image drift, and changed installer/schema/policy/hardware identity on the same image also causes drift. Normal NodePool disruption controls govern replacement. Unknown actual boot-image or historical comparison facts yield an actionable comparison error, not invented drift evidence. Pinning an image does not freeze its installer/profile compatibility or suppress configuration drift.

To disable, remove `sandboxConfig`, remove user-owned runtime fields/requirements, and migrate Pods away from the gVisor RuntimeClass or to another sandbox pool. The controller cleans up only its still-owned NodePool values. New ordinary nodes have inherited sandbox-enabling metadata/installables removed; existing running VMs are **not converted in place**. Plan replacement and Pod disruption budgets, and retain the ordinary system pool. Downgrades to a controller without sandbox support require retiring/migrating sandbox workloads and capacity first; do not assume an older controller preserves the identity and isolation contract.

## Readiness and troubleshooting

Safe, read-only starting points:

```bash
kubectl get gcenodeclass sandbox -o jsonpath='{.status.conditions}'
kubectl get gcenodeclass sandbox -o jsonpath='{.status.images}'
kubectl get nodepool sandbox -o jsonpath='{.spec.template}'
kubectl get runtimeclass gvisor -o yaml
kubectl describe pod sandbox-example
```

- **`ImagesReady=False`, `ImageResolutionFailed`:** check all selectors, public provenance/READY status, exact GPU variant/COS pin, `gvisor-version`, installer availability/platform and network access.
- **`SandboxReady=False`, `SandboxBootstrapUnsupported`:** check safe condition text for missing source fields, invalid typed configuration, public-artifact fetch failure or unknown complete consumer fingerprints. Retry transient network errors; changed schemas need a reviewed controller profile update, not manually patched status or fingerprints.
- **`SandboxConfigurationUnsupported` / no compatible candidates:** check GPU model/version/driver/sharing constraints, effective bootstrap release and reserved metadata overrides. Configuration/artifact failures are not insufficient-capacity signals and do not poison unavailable-offering caches.
- **Scheduling gate:** inspect persisted NodePool runtime label, exactly one permanent taint, requirements and GitOps reconciliation. Verify the admitted Pod has the runtime selector and toleration. A missing RuntimeClass fails admission; an unsafe class can admit a Pod without isolation constraints.
- **Adoption/drift comparison errors:** check controller `compute.disks.get` permission and actual boot-image/identity availability. Do not relabel an ordinary VM as sandboxed or patch identity to bypass guards.

`SandboxReady=True` reports bootstrap capability only, not workload policy, node boot success, runtime isolation or CUDA acceptance. Conditions are periodically refreshed and launch rechecks prerequisites. Never dump or attach raw `kube-env`, source templates, instance metadata, certificates, tokens or ADC settings to diagnostics; use sanitized condition/error text and public artifact identities.

## E2E acceptance specs

`test/suites/sandbox` contains exactly three on-demand provisioning specs: amd64 `n2-standard-2` (60 GiB boot disk), Arm64 `t2a-standard-2` (60 GiB), and `g2-standard-4` with one whole L4 and the default driver (100 GiB). Each uses one uniquely owned pool/class and one runtime workload Pod. The amd64 spec also creates an ordinary negative-control Pod on the same pool, removes both demand Pods, then verifies disable/drift on the original claim without replacement. Cleanup deletes only the spec's Pods before its pool/class, retaining normal finalizers.

All three carry `suite:sandbox`; only the CUDA spec also carries `suite:gpu`. Consequently, the existing **`standard` preset now includes both CPU sandbox cases**, the **`gpu` preset includes the sandbox CUDA case**, and **`--selection sandbox` selects all three**. Preset concurrency is unchanged. To select just these specs through the existing runner, after separate target-environment authorization:

```bash
go run ./hack/e2e-runner --selection sandbox
```

This is a live command, not an offline discovery command. The runner acquires a Kubernetes Lease **even when Ginkgo's `--dry-run` is passed after `--`**; do not use the runner for offline discovery. Writing, compiling or discovering tests is not permission to deploy or execute them.

Before executing any preset that includes sandbox specs:

- Install matching controller/CRDs and the reviewed IAM permissions, and satisfy the artifact/network and strict bootstrap-profile prerequisites above. The known reviewed source profile is `1.35.6-gke.1250001`; unknown consumers fail closed, without a bypass or silent skip.
- Provide a GKE/admin-owned RuntimeClass with handler `gvisor`, a selector containing only `sandbox.gke.io/runtime: gvisor`, and one key-scoped `NoSchedule` toleration for that runtime (Equal `gvisor` or Exists). Its name is discovered read-only; the tests do not create/modify RuntimeClasses or native pools. Missing/unsafe prerequisites fail actionably rather than skipping.
- Supply quota and zonal capacity for the pinned types. For CUDA, install the existing [GPU device plugin/driver prerequisites](gpu-nodes.md); required system DaemonSets must tolerate the sandbox and test-pool taints. The CUDA container requests/limits one GPU, preserves the pinned image's `/cuda-samples/sample` entrypoint, and must terminate successfully with `Test PASSED` in bounded logs.

The pinned multiarch Python probe runs unprivileged without a service-account token. It compares its kernel release with the node's host kernel, checks Service DNS and verified TLS to the Kubernetes API using the public namespace CA, and exercises `emptyDir` I/O. These are runtime/isolation smoke checks, not a security audit or CSI acceptance. The GPU Pod includes that long-running probe, so tests inspect each container instead of waiting for whole-Pod Ready after CUDA exits.

Pool CPU limits are 2/2/4 respectively, with `consolidateAfter: Never`, `expireAfter: Never` and an all-reason zero disruption budget set before demand. The specs assert one original claim. These are bounded operational guards, **not hard cloud allocation or billing guarantees**. Offline compile/label checks do not prove boot, isolation, networking or CUDA success; no live execution is claimed here.

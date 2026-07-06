# Proposal: Typed bootstrap metadata model

- **Status**: Implemented by [#430](https://github.com/cloudpilot-ai/karpenter-provider-gcp/pull/430)
- **Authors**: @dm3ch
- **Created**: 2026-06-09
- **Related Issues**: [#399](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/399), [#408](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/408)
- **Related PRs**: [#430](https://github.com/cloudpilot-ai/karpenter-provider-gcp/pull/430)

---

## Summary

Karpenter now builds GCE instance metadata through a typed model in `pkg/metadata`:

```text
metadata.FromSourceTemplate(sourceMetadata)
-> instance-provider typed/domain mutations
-> metadata.InstanceMetadata.ToComputeMetadata()
-> metadata.InstanceMetadata.ToComputeInstanceLabels()
```

The old `pkg/providers/metadata` helpers were removed. The root metadata package is only an accumulator and render boundary. It parses source template metadata into typed bootstrap surfaces, exposes intent-level mutation helpers, and renders Compute Engine metadata/labels at the API boundary.

Provider policy remains in `pkg/providers/instance`: labels, taints, provisioning model, GPU behavior, disk compatibility, secondary boot disk metadata, kubelet defaults, Spot shutdown behavior, and NodeClass kubelet overlay mapping.

---

## Motivation

The old metadata path mixed several concerns:

- copying GKE source-template metadata;
- patching `kube-env`, `kube-labels`, and `kubelet-config` strings;
- applying provider-owned labels, taints, GPU, disk, max-pods, zone, and provisioning fields;
- overlaying `GCENodeClass.spec.metadata`.

That made ownership hard to review and made source/user metadata merge semantics dependent on string-patching order.

The refactor makes ownership explicit:

- source-template parsing happens once;
- target-owned bootstrap surfaces are rebuilt from provider facts;
- typed helpers mutate metadata state;
- serialization happens once at `compute.Instance` construction.

---

## Implemented Design

### Package Boundary

`pkg/metadata` owns pure metadata modeling:

- `InstanceMetadata`
- `KubeEnv`
- `KubeletArgs`
- `Labels`
- `Taints`
- `CustomMetadata`
- `InstanceLabels`

It has no GCP API calls, no NodeClass policy, no GPU/disk/provisioning helpers, and no source-template discovery.

`pkg/providers/instance` owns provider policy and calls metadata helpers to mutate the typed model.

### Source Template Contract

`metadata.FromSourceTemplate(*compute.Metadata)` retains or parses the source metadata surfaces still needed for GKE bootstrap. The set follows the keys GKE writes into real node-pool instance templates and includes the metadata keys GKE documents as reserved for [`NodeConfig.metadata`](https://cloud.google.com/kubernetes-engine/docs/reference/rest/v1/NodeConfig):

- `cluster-location`
- `cluster-name`
- `cluster-uid`
- `common-psm1`
- `configure-sh`
- `containerd-configure-sh`
- `disable-address-manager`
- `disable-legacy-endpoints`
- `enable-os-login`
- `gci-ensure-gke-docker`
- `gci-metrics-enabled`
- `gci-update-strategy`
- `google-compute-enable-pcid`
- `install-ssh-psm1`
- `instance-template`
- `k8s-node-setup-psm1`
- `kube-env`
- `kubeconfig`
- `kubelet-config`
- `serial-port-logging-enable`
- `startup-script`
- `user-data`
- `user-profile-psm1`
- `windows-startup-script-ps1`

Target-owned fields are not copied from source and are rebuilt by the instance provider:

- `kube-labels`
- `KUBELET_ARGS --node-labels`
- `KUBELET_ARGS --register-with-taints`

Unknown source-template metadata keys are not implicitly passed through. User-provided `GCENodeClass.spec.metadata` remains the explicit custom metadata extension point.

### Custom Metadata and Reserved Keys

`GCENodeClass.spec.metadata` remains supported for custom Compute metadata keys. The CRD rejects the metadata keys GKE documents as reserved for [`NodeConfig.metadata`](https://cloud.google.com/kubernetes-engine/docs/reference/rest/v1/NodeConfig):

- `cluster-location`
- `cluster-name`
- `cluster-uid`
- `common-psm1`
- `configure-sh`
- `containerd-configure-sh`
- `disable-address-manager`
- `enable-os-login`
- `gci-ensure-gke-docker`
- `gci-metrics-enabled`
- `gci-update-strategy`
- `install-ssh-psm1`
- `instance-template`
- `k8s-node-setup-psm1`
- `kube-env`
- `startup-script`
- `user-data`
- `user-profile-psm1`
- `windows-startup-script-ps1`

It also rejects provider-owned bootstrap surfaces that real GKE node templates use and that Karpenter parses or rebuilds rather than passing through as user metadata:

- `kube-labels`
- `kubeconfig`
- `kubelet-config`

The instance provider no longer contains compatibility paths for invalid objects that try to set those reserved keys through `spec.metadata`.

### Provider-owned Mutations

The instance provider sets:

- GCE instance labels;
- GKE `kube-labels` metadata;
- kubelet registration labels in `KUBELET_ARGS --node-labels`;
- `karpenter.sh/unregistered=true:NoExecute`;
- optional `nvidia.com/gpu=present:NoSchedule`;
- max pods;
- provisioning model (`standard` or `spot`);
- zone;
- architecture kube-env entries and machine-family labels;
- OS-distribution kube-env entries;
- GPU accelerator and driver-version labels;
- disk compatibility labels;
- secondary boot disk labels and `SECONDARY_BOOT_DISKS`;
- kubelet reserved resources;
- explicit field-by-field NodeClass kubelet overlay;
- Spot graceful shutdown kubelet overrides.

### Render Boundary

The final instance construction renders through:

```go
metadata.InstanceMetadata.ToComputeMetadata()
metadata.InstanceMetadata.ToComputeInstanceLabels()
```

Metadata item ordering and label serialization are deterministic.

---

## External Patterns

### karpenter-provider-aws

AWS keeps bootstrap data behind typed provider-local options and serializes at a boundary. It also exposes a provider-owned kubelet configuration subset instead of accepting arbitrary upstream kubelet config wholesale.

### karpenter-provider-azure

Azure uses a provider-local NodeClass kubelet subset and maps fields explicitly into the target API. The GCP implementation follows the same pattern by mapping `GCENodeClass.Spec.KubeletConfiguration` field-by-field into `k8s.io/kubelet/config/v1beta1.KubeletConfiguration` inside the instance provider.

---

## Test Coverage

Implemented coverage includes:

- source-template retention for GKE bootstrap metadata keys;
- source-template parsing for `kube-env` and `kubelet-config`;
- dropping source `kube-labels` and rebuilding target labels;
- deterministic `kube-env`, `kube-labels`, kubelet args, and Compute metadata rendering;
- custom metadata replacement for allowed keys;
- typed registration labels and taints;
- GPU labels and optional GPU taint;
- disk compatibility labels;
- secondary boot disk kube labels and kube-env disk paths;
- provisioning model metadata;
- Spot kubelet shutdown overrides;
- NodeClass kubelet configuration overlay;
- nodepooltemplate source metadata copying.

Validation used during #430:

```bash
go test ./pkg/metadata ./pkg/providers/instance/... ./pkg/providers/nodepooltemplate/...
go test ./pkg/...
make verify
```

---

## Behavior Changes

Intentional changes:

- reserved `spec.metadata` bootstrap keys are not supported by instance-provider fallback code;
- source-template `kube-labels` are not inherited;
- source-template `--node-labels` and `--register-with-taints` are not inherited;
- target labels and taints are rebuilt from provider facts;
- metadata and labels render deterministically;
- secondary boot disk kube-env uses the observed GKE comma-separated `SECONDARY_BOOT_DISKS` format.

---

## Risks and Mitigations

| Risk                                                 | Mitigation                                                                                                                                |
|------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------|
| Dropping a source-template field breaks node startup | Retain the observed GKE bootstrap keys explicitly; validate with unit tests and e2e before merge.                                         |
| Target-owned labels or taints regress                | Rebuild both `kube-labels` and registration labels/taints from provider facts; cover GPU, disk, secondary disk, Spot, and max-pods cases. |
| Kubelet config overlay loses supported fields        | Map the NodeClass kubelet subset field-by-field and test the rendered config.                                                             |
| Reserved metadata keys sneak through invalid objects | CRD validation rejects them; provider does not maintain raw fallback support for invalid objects.                                         |

---

## Acceptance Criteria

- [X] `pkg/metadata` owns typed metadata parsing, mutation, and serialization.
- [X] `pkg/providers/instance` owns provider/GKE/Karpenter policy.
- [X] Source-template metadata parsing is explicit and tested.
- [X] Target-owned `kube-labels`, registration labels, and taints are rebuilt.
- [X] `pkg/providers/metadata` is removed.
- [X] `nodepooltemplate` only discovers/copies source template metadata.
- [X] Rendering goes through `ToComputeMetadata()` and `ToComputeInstanceLabels()`.
- [X] Unit tests and `make verify` pass.

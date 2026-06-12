# Proposal: Clean-room typed bootstrap metadata model

- **Status**: Draft
- **Authors**: @dm3ch
- **Created**: 2026-06-09
- **Related Issues**: [#408](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/408), [#393](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/393)
- **Related PRs**: [#394](https://github.com/cloudpilot-ai/karpenter-provider-gcp/pull/394), [#430](https://github.com/cloudpilot-ai/karpenter-provider-gcp/pull/430)

---

## Summary

Karpenter currently builds GCE instance metadata by taking metadata from a GKE-managed source instance template and applying a series of targeted mutations. Some values are fully owned by the provider, some are copied from GKE and patched, and user-provided `GCENodeClass.spec.metadata` is merged late. This has produced bugs and review complexity because ownership is implicit and many operations patch strings directly.

This proposal introduces a clean-room typed metadata model in `pkg/metadata`. The model should make ownership explicit, copy only the GKE source-template fields that the provider intentionally reuses, expose typed setters for provider-owned fields, and serialize to `compute.Metadata` and `compute.Instance.Labels` only at the final instance-construction boundary.

The proposal also captures the guardrail work from #408: user freeform metadata should remain possible for truly custom keys, but reserved GKE/GCE bootstrap keys should not be overrideable through an unstructured map. Useful supported knobs should become typed `GCENodeClass` fields over time.

---

## Motivation

### Problem Statement

Current metadata logic mixes several responsibilities:

- source-template metadata discovery and copying;
- provider-owned bootstrap labels, taints, zone, max-pods, provisioning model, GPU, disk, and kubelet settings;
- GKE-owned bootstrap fields that are retained from the source template;
- user freeform metadata overlay.

Historically this has led to string/regexp patching and unclear ownership. PR #394 fixed one concrete bug: `spec.metadata` values for existing keys were concatenated instead of overriding source-template values. The example was `serial-port-logging-enable=true` from the source template plus `spec.metadata: {serial-port-logging-enable: "false"}` producing `true,false` instead of `false`.

#408 tracks a related design problem: Karpenter creates raw GCE VMs and therefore bypasses GKE NodeConfig validation for reserved metadata keys. Users can currently set GKE bootstrap internals like `kube-env`, `cluster-name`, or `user-data` through `spec.metadata`, even though GKE would reject many of those keys for node pools.

### Goals

- Make metadata ownership explicit in code.
- Use typed structures for provider-owned and explicitly retained metadata surfaces.
- Copy only named source-template fields or sub-fields.
- Avoid arbitrary source-template passthrough.
- Keep user freeform metadata separate from provider-owned fields.
- Preserve #394 override semantics for allowed custom metadata keys.
- Prepare for #408 guardrails and future typed GCENodeClass fields.
- Keep `nodepooltemplate` responsible for source-template discovery and `metadata` responsible only for pure modeling/transformation.

### Non-Goals

- Do not implement the full #408 CRD/API change in the initial metadata refactor unless explicitly scoped later.
- Do not remove support for all freeform user metadata in one step.
- Do not reconstruct all GKE bootstrap metadata from GKE APIs immediately.
- Do not perform GCP API calls from `pkg/metadata`.

---

## External Patterns

### karpenter-provider-aws

Relevant files:

- `pkg/providers/amifamily/bootstrap/bootstrap.go`
- `pkg/providers/amifamily/bootstrap/eksbootstrap.go`
- `pkg/providers/amifamily/bootstrap/nodeadm.go`
- `pkg/apis/v1/ec2nodeclass.go`

Useful patterns:

- AWS passes bootstrap data through an explicit typed `bootstrap.Options` object: cluster facts, kubelet config, labels, taints, custom user data, and other inputs are separate typed fields.
- Provider-owned values are generated from typed fields (`nodeLabelArg`, `nodeTaintArg`, `kubeletExtraArgs`) and serialized at the bootstrap boundary.
- Custom user data is an explicit extension point, parsed/merged through a controlled path rather than treated as a bag that can override internal provider-owned fields.
- Kubelet configuration fields mirror upstream kubelet names where possible.

Apply to GCP:

- Use a typed `metadata.InstanceMetadata` construction path rather than passing loose maps around instance-provider internals.
- Keep `GCENodeClass.spec.metadata` as custom extension metadata, not as input parsed into provider-owned structure.
- Serialize only once at the `compute.Instance` boundary.

### karpenter-core

Relevant files:

- `pkg/apis/v1/labels.go`
- `pkg/apis/v1/taints.go`
- `pkg/utils/pretty/pretty.go`

Useful patterns:

- Use core constants for Karpenter labels and taints (`NodePoolLabelKey`, `NodeRegisteredLabelKey`, `UnregisteredNoExecuteTaint`).
- Use Kubernetes typed taints (`corev1.Taint`) instead of ad hoc strings where possible.
- Serialize maps and taints deterministically for stable tests and diffs.

---

## Live GKE Metadata Findings

A live GKE-managed default-pool source template and a live Karpenter-created VM were inspected with `gcloud` in project `dm3ch-karpenter-dev`.

Source template:

```text
gke-karpenter-e2e-cluste-default-pool-de95d877
region: asia-southeast1
```

Karpenter VM:

```text
karpenter-amd64-od-ncclass-drift-26b3d0-jpsxw
zone: asia-southeast1-a
```

Observed source-template metadata keys:

```text
cluster-location
cluster-name
cluster-uid
configure-sh
disable-legacy-endpoints
gci-metrics-enabled
gci-update-strategy
google-compute-enable-pcid
kube-env
kube-labels
kubeconfig
kubelet-config
serial-port-logging-enable
user-data
```

`origin/main` currently mutates `template.Properties.Metadata` in place, so all source-template keys are copied to provisioned VMs unless overwritten or removed. The inspected Karpenter VM still had these bootstrap-critical keys plus user metadata such as `drift-test`.

This means the clean-room model cannot initially copy only `kube-env` and `kubelet-config`. It needs explicit named fields or copy rules for the other retained GKE bootstrap metadata keys.

Observed source-template `KUBELET_ARGS` flags:

```text
--v
--cloud-provider
--experimental-mounter-path
```

Observed Karpenter VM `KUBELET_ARGS` flags:

```text
--v
--cloud-provider
--experimental-mounter-path
--register-with-taints
```

The inspected Karpenter VM did **not** have `--node-labels`; labels were present in `kube-labels`. Before treating `--node-labels` as required in the new model, verify whether GKE bootstrap consumes `kube-labels` directly and whether existing `--node-labels` patching is required, redundant, or version-dependent.

Observed source-template `kube-labels` were GKE source-pool labels. Observed Karpenter VM `kube-labels` were provider-rebuilt target labels. This supports not copying source `kube-labels` as-is.

---

## Proposal

### Package Boundary

Create/continue `pkg/metadata` as the long-term package for typed metadata modeling.

Rules:

- Pure modeling/transformation only.
- No GCP API calls.
- No source pool or source template discovery.
- No GCE instance creation.
- No arbitrary source-template metadata passthrough.
- Source fields may be retained only through typed fields or explicit copy rules.
- Instance provider decides ownership values; `pkg/metadata` applies and serializes them.

### Initial Structure

```go
type InstanceMetadata struct {
	// Explicitly copied GKE bootstrap metadata items.
	ClusterLocation         string
	ClusterName             string
	ClusterUID              string
	ConfigureScript         ConfigureScript
	DisableLegacyEndpoints  bool
	GCIMetricsEnabled       bool
	GCIUpdateStrategy       string
	GoogleComputeEnablePCID bool
	KubeConfig              KubeConfig
	SerialPortLoggingEnable *bool
	UserData                UserData

	// Parsed/edited bootstrap surfaces.
	KubeEnv       KubeEnv
	KubeLabels    KubeLabels // provider-owned, reconstructed
	KubeletConfig KubeletConfig

	// User-provided extension metadata, after validation.
	CustomMetadata CustomMetadata

	// GCE instance labels, not metadata items.
	InstanceLabels InstanceLabels
}

type KubeEnv struct {
	Entries     KubeEnvEntries
	KubeletArgs KubeletArgs
}

type KubeletArgs struct {
	Verbosity               *int32
	CloudProvider           string
	ExperimentalMounterPath string
	RegisterWithTaints      []corev1.Taint
	MaxPods                 *int32

	// Add typed fields for any additional retained flags.
	// Do not keep generic RawArgs passthrough.
}

type KubeLabels map[string]string

type KubeletConfig struct {
	Config v1alpha1.KubeletConfiguration

	// Add typed fields/helpers for copied source or provider-owned config.
	// Do not keep a generic Raw map.
}

type CustomMetadata map[string]string
type InstanceLabels map[string]string
```

There should be no `SourceTemplate` field in `InstanceMetadata`. The source template is input to a constructor/helper only.

### Source-Copy Contract

The constructor copies only explicitly supported source fields/sub-fields from GKE source template metadata into final typed fields.

Initial explicitly retained source metadata keys:

```text
cluster-location
cluster-name
cluster-uid
configure-sh
disable-legacy-endpoints
gci-metrics-enabled
gci-update-strategy
google-compute-enable-pcid
kube-env
kubeconfig
kubelet-config
serial-port-logging-enable
user-data
```

Do not copy source `kube-labels`. Provider-owned bootstrap labels are reconstructed from NodeClaim, NodeClass, instance type, capacity type, disk compatibility, GPU settings, and other target facts.

Unknown source-template metadata keys are dropped unless added to the explicit retained list with a typed field or copy rule.

### Ownership / Setters

Instance provider owns and sets:

- GCE instance labels (`compute.Instance.Labels`)
- bootstrap `kube-labels`
- provider taints (`karpenter.sh/unregistered`, GPU taint)
- max pods
- provisioning model
- zone
- instance type architecture/family labels
- GPU labels/taints
- disk compatibility labels
- secondary boot disk labels/env entries
- kubelet-config provider defaults and Spot shutdown overrides

Candidate methods:

```go
func FromSourceTemplate(*compute.Metadata) (*InstanceMetadata, error)
func (m *InstanceMetadata) SetInstanceLabels(InstanceLabels)
func (m *InstanceMetadata) SetBootstrapLabels(KubeLabels)
func (m *InstanceMetadata) SetRegisterWithTaints([]corev1.Taint)
func (m *InstanceMetadata) SetMaxPods(int32)
func (m *InstanceMetadata) SetProvisioningModel(string)
func (m *InstanceMetadata) SetZone(string)
func (m *InstanceMetadata) SetInstanceType(arch, family string)
func (m *InstanceMetadata) SetGPU(...)
func (m *InstanceMetadata) SetDiskCompatibilityLabels(KubeLabels)
func (m *InstanceMetadata) MergeKubeletConfig(...)
func (m *InstanceMetadata) SetCustomMetadata(CustomMetadata) error
func (m *InstanceMetadata) ToComputeMetadata() *compute.Metadata
func (m *InstanceMetadata) ToComputeLabels() map[string]string
```

### Custom Metadata and #408

`GCENodeClass.spec.metadata` should remain as custom extension metadata for now, not as input parsed into provider-owned structure.

Flow:

```go
metadata := metadata.FromSourceTemplate(sourceTemplate)
metadata.SetProviderOwnedFields(...)
metadata.SetCustomMetadata(validatedNodeClassMetadata)
```

`SetCustomMetadata` should be able to add/override only keys that are not reserved, retained source fields, or provider-owned structured fields. Important supported knobs should graduate from freeform metadata into typed `GCENodeClass` fields over time.

GKE NodeConfig `metadata` is freeform, but GKE rejects reserved keys. Terraform's `google_container_node_pool.node_config.metadata` follows that GKE model: users can provide metadata, but GKE applies server-side reserved-key validation. Karpenter bypasses that validation by creating raw GCE VMs, so the provider should eventually enforce equivalent guardrails.

GKE reserved keys from NodeConfig docs:

```text
cluster-location
cluster-name
cluster-uid
configure-sh
containerd-configure-sh
enable-os-login
gci-ensure-gke-docker
gci-metrics-enabled
gci-update-strategy
instance-template
kube-env
startup-script
user-data
disable-address-manager
windows-startup-script-ps1
common-psm1
k8s-node-setup-psm1
install-ssh-psm1
user-profile-psm1
```

Initial #408 design direction:

| Key / group                                                             | Proposed direction                                                                             |
|-------------------------------------------------------------------------|------------------------------------------------------------------------------------------------|
| `serial-port-logging-enable`                                            | Keep allowlisted custom override or introduce typed field. Real org-policy use case from #394. |
| `disable-legacy-endpoints`                                              | Likely allow or type after validating GKE/Terraform behavior.                                  |
| `google-compute-enable-pcid`                                            | Research before allowing typed field; likely image/runtime tuning.                             |
| `gci-metrics-enabled`, `gci-update-strategy`                            | Block freeform override; source-retained/provider-owned.                                       |
| `cluster-location`, `cluster-name`, `cluster-uid`                       | Block freeform override; source/provider-owned.                                                |
| `configure-sh`, `user-data`, `kube-env`, `kubelet-config`, `kubeconfig` | Block freeform override; expose typed fields only after product decision.                      |
| `instance-template`, `startup-script`, Windows setup keys               | Block freeform override.                                                                       |
| Unknown non-reserved custom keys                                        | Keep freeform, subject to syntax/size validation.                                              |

---

## Regression Guards

### PR #394

The new model must preserve #394 semantics for allowed custom metadata keys:

- user custom metadata overrides matching retained metadata keys;
- override replaces exactly and never concatenates with commas;
- `serial-port-logging-enable=true` from source plus user override `false` serializes as exactly `false`;
- new user keys are added;
- empty user values preserve the documented current behavior unless explicitly changed.

Equivalent tests should exist under `pkg/metadata` before switching instance provider to the clean-room model.

### Bootstrap key retention

Tests should use a fixture modeled after the live GKE source-template metadata inventory and assert that all explicitly retained keys are present after serialization. Source `kube-labels` should be ignored and reconstructed.

---

## Implementation Phases

### Phase 1 — Structure definitions

- Add `pkg/metadata/doc.go` and initial type definitions.
- Keep provider wiring unchanged.

### Phase 2 — Parser/serializer tests

- Add tests for source-copy contract.
- Add tests for dropped unknown source keys.
- Add tests for ignored source `kube-labels`.
- Add #394 custom override tests.

### Phase 3 — Typed parsers/serializers

- `KubeLabels` parse/string, deterministic.
- `corev1.Taint` parse/string for kubelet flag values.
- `KubeletArgs` parse/string for supported flags only.
- `KubeEnv` parse/string for explicit entries and kubelet args.
- `KubeletConfig` parse/string with typed helpers.

### Phase 4 — Instance provider integration

- `nodepooltemplate` returns copied raw `*compute.Metadata`.
- `instance` calls `metadata.FromSourceTemplate`.
- `instance` applies provider-owned fields through typed methods.
- Serialize once to `compute.Metadata` and `compute.Instance.Labels`.

### Phase 5 — Remove old helpers

Remove superseded `pkg/providers/metadata` helpers once equivalent typed behavior is covered.

---

## Test Plan

### Unit Tests

- `go test ./pkg/metadata`
- Parser/serializer tests for every structured type.
- Source-copy tests using live-template-shaped fixture.
- Custom metadata override tests from #394.
- Reserved/internal key validation tests if #408 is implemented in this PR.

### Integration / E2E

Before merge, run bootstrap-sensitive e2e coverage:

- normal provisioning;
- custom metadata override for `serial-port-logging-enable`;
- kubelet config / max pods;
- disk compatibility labels;
- secondary boot disks;
- GPU labels/taints;
- Spot provisioning model;
- drift/expiration scenarios.

---

## Risks and Mitigations

| Risk                                                      | Likelihood | Impact | Mitigation                                                                           |
|-----------------------------------------------------------|-----------:|-------:|--------------------------------------------------------------------------------------|
| Dropping a GKE bootstrap metadata key breaks node startup |     Medium |   High | Start with explicit retention of all observed bootstrap-critical keys; e2e validate. |
| Typed model misses version-dependent GKE metadata fields  |     Medium | Medium | Use live inventory fixtures; allow adding explicit retained fields after evidence.   |
| Changing `spec.metadata` behavior breaks users            |     Medium | Medium | Preserve freeform for non-reserved keys; separate #408 validation/API changes.       |
| `--node-labels` assumption is wrong                       |     Medium | Medium | Verify with e2e and live nodes before requiring it in typed model.                   |

---

## Open Questions

1. Should `serial-port-logging-enable` remain an allowlisted custom metadata key or become a typed GCENodeClass field?
2. Should Karpenter mimic GKE's reserved-key denylist exactly, or also block observed internals like `kubelet-config` and `kubeconfig`?
3. Are `kube-labels` alone sufficient for GKE bootstrap on all supported versions, or do some versions require `--node-labels`?
4. Which GKE/Terraform `node_config` fields should GCENodeClass mimic instead of exposing metadata keys?
5. Should empty custom metadata values continue to be ignored, delete keys, or be rejected?

---

## Acceptance Criteria

The clean-room metadata refactor is complete when:

- [ ] `pkg/metadata` owns typed metadata parsing, mutation, and serialization.
- [ ] Source-template metadata retention is explicit and tested.
- [ ] Provider-owned fields are set through typed methods, not inline regex/string patches in `instance`.
- [ ] #394 custom metadata override semantics are preserved.
- [ ] `nodepooltemplate` only discovers/copies source template metadata.
- [ ] Bootstrap-sensitive unit and e2e validation passes.

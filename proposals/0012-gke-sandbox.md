# Proposal: GKE Sandbox (gVisor)

- **Status**: Provisional
- **Authors**: CloudPilot AI contributors
- **Created**: 2026-10-22
- **Related Issues**: [#504](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/504)

## Summary

Add optional `GCENodeClass.spec.sandboxConfig.type: GVISOR` for native GKE Sandbox scheduling and reconstructed-node bootstrap. Omission preserves ordinary capacity. This record captures the design tradeoffs, not maintainer acceptance or a claim of live validation. Configuration and troubleshooting belong in [Sandbox nodes](../docs/sandbox-nodes.md) and the [minimal example](../docs/examples/sandbox.md).

## Motivation

Ordinary COS bootstrap does not enable gVisor. Runtime selection, scheduling and historical VM identity must agree: a sandbox workload must not receive ordinary capacity, while ordinary workloads without the sandbox toleration must not trigger sandbox provisioning. Copying a source pool's settings would also copy hardware-specific policy and mutable runtime artifacts.

MicroVM support, RuntimeClass administration and general workload security certification are non-goals. MicroVM support is tracked separately in [#707](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/707).

## Proposal

### Terraform-shaped opt-in API

**Decision:** use the GKE/Terraform-shaped `sandboxConfig: {type: GVISOR}` object, with omission meaning ordinary capacity. Restrict sandboxing to supported public GKE COS images and compatible whole-GPU configurations.

**Why / alternatives:** a boolean would lose the runtime type; aliases or a separate installer-version field would introduce competing configuration identities. Unsupported runtime values are rejected rather than treated as ordinary capacity.

**Consequences:** controller and CRDs must be upgraded together before opt-in. Runtime artifact selection follows the selected image, not a user-selected installer that might be incompatible. This is an optional feature, not a breaking migration for non-users.

### Public artifacts, not a native sandbox pool prerequisite

**Decision:** resolve actual public COS images and each image's immutable public gVisor installer digest, retaining existing ordinary bootstrap-pool discovery for cluster-specific material. Discover published sandbox GPU image variants rather than synthesizing names.

**Why / alternatives:** requiring a native sandbox source pool adds infrastructure and leaks source hardware policy. Guessing installer versions, using mutable `latest`, or falling back to ordinary GPU images cannot preserve exact image/build pins or runtime identity.

**Consequences:** no native sandbox pool is required, but artifact availability, provenance, architecture and outbound network access become explicit sandbox prerequisites. Missing compatible artifacts fail closed. The controller maintains hardware/security policy separately from artifact discovery.

### Strict full-consumer fingerprints over upgrade availability

**Decision:** require the effective bootstrap/kubelet release's complete consumer hashes to match a reviewed profile, together with explicit hardware-policy identity. The initial reviewed fixture is GKE `1.35.6-gke.1250001`.

**Why / alternatives:** master-version checks, semver floors and text markers cannot detect changed bootstrap consumer semantics. Copying settings from a native template does not establish their safety on different hardware. Matching consumers is not discovery of GKE's complete native hardware/security policy.

**Consequences:** unknown consumers block new sandbox provisioning, including after a GKE upgrade, until reviewed compatibility evidence permits a controller update. There is no bypass switch. This prioritizes fail-closed behavior over provisioning availability and creates an ongoing compatibility-maintenance obligation; image pins do not remove it.

### GKE / administrator RuntimeClass ownership

**Decision:** leave RuntimeClasses to GKE or administrators and accept any class name targeting handler `gvisor` with safe scheduling. Rely on admission for Pod selector, tolerations and overhead, and on core Karpenter for overhead accounting.

**Why / alternatives:** controller-owned RuntimeClasses or literal-name assumptions would conflict with cluster policy and custom names. Duplicating overhead or Pod mutation would duplicate Kubernetes responsibilities.

**Consequences:** no new RuntimeClass RBAC or automatic repair. Operators must provide safe scheduling settings. Tolerating the sandbox taint allows ordinary Pods onto sandbox nodes but does not itself select gVisor.

### Persisted NodePool field ownership, not scheduler mutation

**Decision:** reconcile the native runtime label and permanent taint onto NodePool templates, recording independent ownership of controller-added fields. Gate scheduling and launch on persisted consistency; remove only still-owned canonical fields on disable/reference changes.

**Why / alternatives:** scheduler-only mutation hides the effective contract from users and other controllers. Overwriting conflicts or removing all runtime fields on disable would destroy user/GitOps ownership.

**Consequences:** preexisting identical user values and unrelated fields survive; conflicts require operator correction. GitOps users can declare both canonical fields before reconciliation. Existing VMs are not converted in place; disruption controls govern replacement.

### VM-factual historical identity, not mutable caches

**Decision:** persist runtime, immutable installer, bootstrap schema/policy/hardware and launch-owner identity on VMs. Get/List/adoption/drift use VM facts, including the actual boot-disk source image, rather than today's NodeClass or instance-type cache.

**Why / alternatives:** reconstructing identity from current desired state can misidentify ordinary or stale VMs as sandbox capacity. Filtering no-longer-launchable instances out of discovery would break lifecycle and garbage collection.

**Consequences:** historical instances remain discoverable, malformed identity fails closed, and unknown comparison facts produce errors rather than invented drift. Sandbox adoption/drift requires `compute.disks.get`. Configuration/image/installer/profile changes can cause drift without converting the running node.

## Alternatives Considered

The rejected alternatives and their tradeoffs are recorded alongside each decision above. None requires a new general-purpose deployment runner or automatic native-pool creation.

## Opt-in setup and lifecycle

See [prerequisites](../docs/sandbox-nodes.md#prerequisites-and-support-boundary), [GitOps ownership](../docs/sandbox-nodes.md#gitops-ownership) and [disabling](../docs/sandbox-nodes.md#identity-drift-and-disabling) for IAM/CRD updates, compatibility, network access and workload migration before disable/downgrade. Ordinary installations have no sandbox opt-in action.

## Acceptance Criteria

Offline regression tests and zero-node native-template comparisons establish API, artifact, bootstrap-policy, scheduling ownership and lifecycle contracts, not reconstructed-node boot, runtime isolation or CUDA execution. The three [sandbox E2E specs](../docs/sandbox-nodes.md#e2e-acceptance-specs) cover amd64/Arm64 runtime smoke checks, amd64 disable/drift and one L4 CUDA workload; they still require separately authorized live execution. Broader GPU, storage and system-workload compatibility remains a live acceptance responsibility. Writing or compiling these tests does not authorize cloud, Kubernetes or IAM mutations.

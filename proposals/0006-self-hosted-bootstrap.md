# Proposal: Self-Hosted Cluster Support

- **Status**: Draft
- **Authors**: @hakman
- **Created**: 2026-07-07
- **Related Issues**: [#297](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/297)

---

## TL;DR

```yaml
# Operator settings (helm values / env)
PROVISION_MODE: self-hosted
CLUSTER_NAME: mycluster.example.com    # exact distribution cluster name
---
apiVersion: karpenter.k8s.gcp/v1alpha1
kind: GCENodeClass
metadata:
  name: self-hosted
spec:
  imageSelectorTerms:
    - id: projects/ubuntu-os-cloud/global/images/ubuntu-2404-noble-amd64-v20260601
  networkConfig:
    subnetwork: projects/my-project/regions/us-central1/subnetworks/nodes
  kubeletConfiguration:
    maxPods: 110                       # must match the startup script's kubelet
  shieldedInstanceConfig:
    enableVtpm: true                   # e.g. kOps TPM node authorization
  metadata:
    kops-k8s-io-instance-group-name: nodes-us-central1-a
  networkTags:
    - mycluster-example-com-node
  startupScript: |
    #!/bin/bash
    # distribution bootstrap script (e.g. kOps nodeup); owns node join end to end
    ...
```

One deployment-wide setting, one NodeClass field. Karpenter launches the instances; the startup script joins them to the cluster.

---

## Summary

Karpenter GCP only works against GKE clusters: it reads cluster configuration from the Container API and clones bootstrap metadata (`kube-env`, `kubeconfig`, startup scripts) from an existing GKE node pool template. Self-hosted clusters on GCP (e.g. kOps) have no GKE cluster object and no node pool to clone from, so they cannot use Karpenter at all.

This proposal adds a `PROVISION_MODE` operator setting with values `gke` (default, current behavior) and `self-hosted`. In `self-hosted` mode the provider never calls the Container API; node bootstrap is owned entirely by a startup script the user supplies through a new `GCENodeClass` field, `spec.startupScript`, written verbatim to the instance's `startup-script` metadata key, which the GCE guest agent executes natively on every supported image (no cloud-init required). Whether the control plane is GKE is a property of the deployment (like `CLUSTER_NAME`), so it is declared once at the deployment, not inferred per NodeClass.

This mirrors the AWS provider's `amiFamily: Custom` passthrough, which self-hosted distributions already build on today. A reference implementation of the same approach for Azure exists in [Azure/karpenter-provider-azure#1765](https://github.com/Azure/karpenter-provider-azure/pull/1765).

---

## Motivation

### Problem Statement

Every node launch today depends on GKE in three ways:

1. **Cluster discovery**: network, subnetwork, pod secondary range, zones, and private cluster settings come from `Projects.Locations.Clusters.Get` (`pkg/providers/gke/gke.go`).
2. **Bootstrap material**: `pkg/providers/nodepooltemplate` selects a RUNNING GKE node pool and copies its instance template metadata; the instance provider then patches `kube-env`, `kube-labels`, and `kubelet-config` (proposal 0004).
3. **Node identity**: `gke-<cluster>-node` firewall tags and GKE readiness-gate labels assume GKE-created firewall rules and system DaemonSets.

On a self-hosted cluster all three fail. Self-hosted distributions already own node bootstrap through their own join mechanisms (e.g. kOps nodeup) and only need Karpenter to launch correctly-shaped instances. This is the same division of labor such distributions use today with the unmodified AWS provider.

### Goals

- In `self-hosted` mode, the provider makes zero Container API calls and requires no `container.googleapis.com` IAM permissions; misconfigured GKE-only options fail at startup.
- Any distribution whose bootstrap fits in a startup script works without distribution-specific code in the provider; kOps is the reference validation target (per issue #297).
- `gke` mode behavior is unchanged; existing deployments upgrade with no action.

### Non-Goals

- **Merging or augmenting GKE bootstrap.** In `self-hosted` mode the startup script replaces bootstrap entirely. If augmentation of GKE bootstrap is ever wanted, it will be a separate field, never a semantic change to `startupScript`.
- **Mixing modes in one deployment.** A cluster is either GKE or self-hosted; the mode is deployment-wide. Per-NodeClass `startupScript` on GKE clusters is a possible future direction, not part of this proposal.
- Image-owned bootstrap with an empty `startupScript` (a minimal startup script is required).
- Windows nodes, secret storage for startup scripts, per-node templating.
- IPv6 / dual-stack nodes: `networkConfig` models neither stack type nor IPv6 access configs today, so IPv6-only clusters are out of scope for phase 1.
- Distribution-side integration work (e.g. kOps generating `GCENodeClass`/`NodePool` from InstanceGroups, or MIG-less node identity in kops-controller). Tracked in the respective projects.
- Automated self-hosted e2e in CI (initial validation is manual).

---

## Proposal

### Overview

```
gke mode (today, unchanged):            self-hosted mode (new):
  GKE API -> cluster config               NodeClass -> subnetwork, image id, script
  node pool template -> kube-env          startupScript -> startup-script metadata
  Karpenter patches kubelet config        startup script owns kubelet, CNI, join
```

`PROVISION_MODE` selects the mode for the whole deployment. All mode gates read the option through a single helper so the branch points stay greppable and each carries a comment naming this proposal.

### Operator Options

- New option `--provision-mode` / `PROVISION_MODE`: `gke` (default) | `self-hosted`.
- Startup validation in `self-hosted` mode rejects `--default-nodepool-template-name` / `DEFAULT_NODEPOOL_TEMPLATE_NAME` (bootstrap-pool pinning is meaningless without GKE pools). `--default-nodepool-service-account` / `DEFAULT_NODEPOOL_SERVICE_ACCOUNT` remains valid in both modes: it is the deployment-wide node service account fallback used by `resolveServiceAccount` (`spec.serviceAccount` → option → compute default SA), not a GKE-only setting.
- `--project-id` / `PROJECT_ID`, `--cluster-name` / `CLUSTER_NAME`, and `--cluster-location` / `CLUSTER_LOCATION` remain required in both modes. `CLUSTER_LOCATION` accepts a region or a zone in both modes (zonal GKE clusters already pass a zone today); in `self-hosted` mode a zone is normalized to its region (`us-central1-a` → `us-central1`) and zones are always listed region-wide — the option never constrains placement, which belongs to NodePool `topology.kubernetes.io/zone` requirements.

### Container API Isolation

"Zero Container API calls" is a load-bearing guarantee, so the design names every current caller — plus the launch path that depends on GKE-created resources — and how each is handled in `self-hosted` mode:

| Caller | Handling |
|--------|----------|
| nodepooltemplate controller (`Sync`/`EnsureFallbackPool`) | controller not registered |
| `GetSourceTemplateMetadata` in instance launch (Compute-API-only itself, but reads templates discovered by the GKE-backed nodepooltemplate sync) | skipped (self-hosted metadata builder, below) |
| `GetClusterConfig` for network/private-cluster defaults in launch | replaced by NodeClass-sourced values (below) |
| `ResolveClusterZones`, called from both instance launch **and** `instancetype.List` | `self-hosted` implementation lists Compute API zones for the region; today's compute listing only runs after a successful cluster fetch, so this is a rewire, not a reorder |
| `GetClusterConfig`/`GetServerConfig` for image `channel:` resolution, reachable through the nodeclass status image reconciler even for invalid NodeClasses | image provider returns a hard resolution error for non-`id` terms in `self-hosted` mode before touching any GKE path |

The `gke.Provider` interface gets a `self-hosted` implementation backed only by the Compute API; acceptance is verified with `container.googleapis.com` IAM denied.

### API Changes

One new optional field on `GCENodeClassSpec`:

```go
// StartupScript is the complete startup script written verbatim to the
// instance's "startup-script" metadata key, executed natively by the GCE
// guest agent. Only valid when the operator runs with
// PROVISION_MODE=self-hosted; the script owns node bootstrap end to end.
// Do not embed secrets: the value is readable through the instance
// metadata server by any process on the node. Fetch secrets by reference
// at boot instead.
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=131072
// +optional
StartupScript *string `json:"startupScript,omitempty"`
```

The 128 KiB limit is deliberate: OpenAPI `maxLength` counts characters while GCE's limits are in **bytes** — 256 KB per value, 512 KB per instance (shared with `cluster-name`, `spec.metadata`, and karpenter keys) — and values near 256 KiB break client-side `kubectl apply` (the last-applied annotation cap). Large startup scripts should fetch by reference.

Validation splits by what each layer can see (CEL cannot read operator options):

**CEL (in-object invariants, mode-independent, spec-level):**

| Rule | Reason |
|------|--------|
| `startupScript` set ⇒ `kubeletConfiguration.maxPods` set and ≥ 1 | Karpenter cannot parse the startup script; the provisioner must state the value binpacking should use. Required rather than defaulted: a silent default that disagrees with the startup script's kubelet is a production binpacking bug, while a missing field is an apply-time error; self-hosted NodeClasses are typically provisioner-generated, so requiring it costs nothing |
| `startupScript` set ⇒ `networkConfig.subnetwork` required | no GKE cluster to discover the network from |
| `startupScript` set ⇒ `disks` contains a boot disk | an empty `disks` list is not defaulted and fails only at launch (`Instances.Insert` 400 "No disks are specified"); require it at apply time |
| `startupScript` set ⇒ every `imageSelectorTerms` entry uses `id` | alias/family/channel resolution is GKE-catalog-shaped (see Non-Goals) |
| `startupScript` set ⇒ `kubeletConfiguration` limited to the binpacking inputs (`maxPods`, `podsPerCore`, `systemReserved`, `kubeReserved`, `evictionHard`) | every other field reaches nodes only through the GKE `kubelet-config` injection this mode removes; accepting them silently would repeat #398 (accepted-but-never-applied). CEL cannot express "any field outside this set", so the rule enumerates the complement, and a unit test asserts the two sets cover every `kubeletConfiguration` field so new fields cannot slip past |
| `startupScript` set ⇒ `autoGPUTaint` unset/false | the taint is applied through bootstrap metadata this mode does not render |
| `startupScript` set ⇒ no `disks[].secondaryBootImage`/`secondaryBootMode` | the container image cache requires GKE kube-labels/kube-env wiring and node tooling; the disk would attach, cost money, and never be used |

**Runtime (mode-aware, NodeClass status controller):** a new `BootstrapConfigReady` condition in the Ready set. `self-hosted` mode requires `startupScript` ("set spec.startupScript"); `gke` mode rejects NodeClasses that set it. A NotReady NodeClass blocks provisioning from that NodeClass only.

The reserved metadata key blocklist on `spec.metadata` (#446) is **unchanged**: `startup-script`, `user-data`, and `cluster-name` stay provider-owned keys. The NodeClass hash version stays `v4`; the new field participates in the hash, and absent-on-existing-objects means existing hashes are unaffected. Any change to `startupScript` therefore drifts and rolls every node provisioned from that NodeClass (the same contract as AWS `userData`); distributions that rotate bootstrap scripts on upgrade should expect fleet rolls.

### Launch Path (`self-hosted` mode)

**Skipped:**

- Bootstrap pool discovery and source template metadata. Instance metadata is produced by a dedicated self-hosted metadata builder that renders **only** `startup-script`, `cluster-name`, `spec.metadata` keys, and karpenter-owned keys, never `kube-env`, `kube-labels`, or `kubelet-config` (the current typed model always renders those, so this is a separate construction path, not a nil source).
- The Spot shutdown kubelet patch (owned by the startup script; see Node Registration Contract).
- `gke-<cluster>-node` firewall tags (users supply `networkTags`, e.g. kOps role tags).
- GKE readiness-gate label inheritance.
- `goog-gke-*` instance labels (`goog-gke-node`, cost-management, provisioning-model, `goog-gke-cluster-id-base32`; the last requires the GKE cluster ID, which does not exist here).
- GPU driver metadata and labels. `gpuDriverVersion` is **ignored** in `self-hosted` mode (it is CRD-defaulted to `default`, so rejecting non-`disabled` values would reject every NodeClass); driver installation and GPU taints are owned by the startup script, and `autoGPUTaint: true` is rejected by CEL (see the validation table). GPU nodes require an `id`-pinned GPU-capable image plus drivers installed by the startup script.

**Changed source, same behavior:**

- Network interface: from `networkConfig.subnetwork` (network resolved from the subnetwork resource via the Compute API; the same derivation applies to each additional interface, which today falls back to the GKE cluster network). Nodes are private by default: unset `networkConfig.enablePrivateNodes` means `true` in `self-hosted` mode (today's default comes from the GKE private-cluster config; defaulting to public IPs on a silent fallback would be a security regression). Alias IP range only when `subnetRangeName` is set.
- Zones: Compute API zone listing for the region (see Container API Isolation).
- `cluster-name` **instance metadata**: written verbatim from the `CLUSTER_NAME` option (`gke` mode inherits it from the source template). Distributions that authorize joining nodes by reading instance metadata back through the Compute API as a trust anchor (e.g. kOps' TPM-based node authorizer) reject instances whose `cluster-name` is missing or does not match; for them, `CLUSTER_NAME` must equal the distribution's cluster name exactly. Distributions that do not read the key ignore it. Provider-owning it (instead of allowing it in `spec.metadata`) matches that trust model: cluster membership is asserted by the provider at instance creation, not by the NodeClass author.
- Cluster-name **GC label**: self-hosted cluster names are often FQDNs (`mycluster.example.com`), but GCE label values only allow `[a-z0-9_-]`, and today the raw name is written as the label value and used in the list filter, so an FQDN would fail `Instances.Insert` outright. The label value and the `syncInstances` filter therefore use a sanitized form (lowercase, invalid characters mapped to `-`, truncated to 63), while the exact name lives only in the `cluster-name` metadata key. Sanitizing unconditionally in both modes keeps a single code path and changes nothing for GKE, whose cluster names are already label-safe. Sanitization is lossy (`a.b.com` and `a-b.com` both map to `a-b-com`; truncation collides long names sharing a 63-character prefix), and the GC filter matches on this label, so two same-project clusters whose sanitized names collide would garbage-collect each other's instances; the self-hosted guide documents that cluster names in one project must remain distinct after sanitization. Startup validation fails if sanitization would produce an empty value.

**Unchanged:** instance naming (`karpenter-<nodeclaim>`), disks, shielded/confidential VM config, karpenter identity labels (with the sanitized cluster value above), Spot scheduling, provider ID (`gce://<project>/<zone>/<name>`), pricing, consolidation, and the interruption controller's mechanism (but see the Spot item in the contract: detection is conditional on startup-script behavior).

### Controllers

- **nodepooltemplate controller**: not registered in `self-hosted` mode.
- **CSR approver controller**: not registered in `self-hosted` mode. It currently auto-approves every pending kubelet client CSR based on signer/usages alone; on a self-hosted cluster that would bypass the distribution's own node authorization (e.g. kOps' TPM verifier), the opposite of this design's trust model.
- **nodeclass status controller**: reconciles the `BootstrapConfigReady` condition; the image reconciler enforces `id`-only resolution in `self-hosted` mode (see Container API Isolation).

### Node Registration Contract

The contract any distribution must satisfy, documented with runnable examples (`docs/examples/`):

- The kubelet must register with the `karpenter.sh/unregistered:NoExecute` taint (upstream contract). Current karpenter core logs an error and proceeds when the taint is missing, but without it pods can schedule before Karpenter syncs the NodeClaim's labels and taints onto the node — treat it as required.
- The node's provider ID must be `gce://<project>/<zone>/<instance-name>` (the GCP cloud-controller-manager default).
- The startup script owns kubelet configuration, CNI, container runtime, and node labels/taints. `kubeletConfiguration.maxPods` is required and must match the kubelet it configures, so binpacking stays correct.
- `kubeletConfiguration.systemReserved`, `kubeReserved`, and `evictionHard` should mirror the startup script's kubelet settings where they differ from the defaults, since they feed Karpenter's allocatable/overhead math, which otherwise assumes the GKE reservation model and would misestimate node capacity.
- The startup script must not embed secrets: instance metadata is readable by any process on the node (and by pods, unless metadata access is blocked). Fetch credentials by reference at boot.
- **Spot**: interruption detection watches for the Node Ready condition kubelet emits during graceful shutdown. In `gke` mode Karpenter configures this itself; in `self-hosted` mode the startup script must configure kubelet graceful node shutdown with a non-zero `shutdownGracePeriod`, or Spot preemptions go undetected.

Distribution-specific requirements live in the docs, not in the provider. For the reference distribution (kOps): `CLUSTER_NAME` equal to the kOps cluster name, vTPM enabled via `shieldedInstanceConfig` (TPM node authorization), `kops-k8s-io-instance-group-name` in `spec.metadata` (not a reserved key), the nodeup script in `startupScript` (kOps' startup-script bootstrap variant), and role network tags in `networkTags`.

### IAM Requirements (`self-hosted` mode)

The controller needs only Compute Engine permissions — explicitly no `container.googleapis.com` role: `compute.instances.{create,get,list,delete,setMetadata,setLabels}`, `compute.disks.create`, `compute.subnetworks.{get,use,useExternalIp}`, `compute.zones.list`, `compute.machineTypes.list`, `compute.images.{get,useReadOnly}`, and `iam.serviceAccounts.actAs` on the node service account. The self-hosted guide documents this list; distributions typically do not grant their worker SAs any project role, so a dedicated controller SA is the expected deployment shape.

---

## Risks and Mitigations

| Risk | Likelihood | Impact | Mitigation |
|------|------------|--------|------------|
| "Node never joined" reports for startup-script bugs | High | Support noise | Documented registration contract + example; `BootstrapConfigReady` distinguishes provider misconfig from startup-script bugs |
| Startup-script kubelet diverges from NodeClass kubelet hints | Medium | Binpacking drift, missed Spot interruptions | Contract spells out maxPods, reserved resources, and graceful shutdown; provisioners generate both sides from one source |
| A GKE user sets `startupScript` expecting augmentation | Medium | Confusion | Rejected in `gke` mode by the status controller with a message; non-goal reserves a separate field for augmentation |
| Mode gates decay under refactoring | Medium | GKE regressions | Single mode helper; each gate commented; `gke`-mode tests assert unchanged metadata output |
| Same-project clusters with colliding sanitized names | Low | Cross-cluster GC deletes the other cluster's Karpenter nodes | Documented requirement: cluster names in one project must stay distinct after sanitization |

---

## Test Plan

### Unit Tests

- Options: `self-hosted` mode rejects `--default-nodepool-template-name`; unknown mode rejected; `CLUSTER_NAME` sanitization validated at startup; zonal `CLUSTER_LOCATION` normalized to its region.
- CEL: maxPods (set and ≥ 1) / subnetwork / boot-disk requirements, `id`-only image terms, node-delivery-only kubelet fields rejected (with an exhaustiveness check that the allowlist plus the rejected set cover every `kubeletConfiguration` field), `autoGPUTaint` and secondary boot disks rejected, `startupScript` size limits; hash stability for objects without the new field.
- Instance creation (`self-hosted` mode): `spec.startupScript` written byte-for-byte to the `startup-script` metadata key; exact `cluster-name` metadata + sanitized GC label; **absence** of `kube-env`, `kube-labels`, `kubelet-config`, `gke-*` tags, `goog-gke-*` labels, and readiness labels; subnetwork honored; private-by-default networking.
- Instance creation (`gke` mode): existing tests unchanged, proving no behavior change.
- Controllers: nodepooltemplate and CSR approver not registered in `self-hosted` mode; `BootstrapConfigReady` requires `startupScript` in `self-hosted` mode and rejects it in `gke` mode; image reconciler errors on non-`id` terms without touching GKE paths.

### E2E / Integration Tests

- Manual validation against at least one self-hosted distribution (kOps, per #297): provision, register, consolidate, Spot interruption (with kubelet graceful shutdown enabled by the startup script). Automated self-hosted e2e is a follow-up.

---

## Acceptance Criteria

The feature is complete when:

- [ ] `PROVISION_MODE` option exists (`gke` default, `self-hosted`); `self-hosted` rejects `--default-nodepool-template-name`, validates `CLUSTER_NAME` sanitization at startup, and normalizes a zonal `CLUSTER_LOCATION` to its region.
- [ ] `GCENodeClass.spec.startupScript` exists with size limits and its CEL rules; `GCENodeClassHashVersion` stays `v4` with hash-stability tests.
- [ ] `BootstrapConfigReady` joins the Ready set: `startupScript` required in `self-hosted` mode, rejected in `gke` mode.
- [ ] The self-hosted metadata builder renders only `startup-script`, `cluster-name`, `spec.metadata` keys, and karpenter-owned keys.
- [ ] nodepooltemplate and CSR approver controllers are not registered in `self-hosted` mode.
- [ ] Zone and image resolution make zero Container API calls in `self-hosted` mode.
- [ ] The GC label carries the sanitized cluster name; `cluster-name` metadata carries the exact name.
- [ ] Nodes default to private networking; each NIC's network derives from its subnetwork.
- [ ] A self-hosted cluster (validated with kOps) provisions and consolidates nodes with `container.googleapis.com` IAM denied for the controller's service account.
- [ ] All existing GKE e2e suites pass unchanged with `PROVISION_MODE` unset.
- [ ] Docs: self-hosted guide with the registration contract, the IAM requirements list, the sanitized-cluster-name uniqueness requirement, at least one distribution example, and chart guidance for non-GKE clusters (`controller.priorityClassName` defaults to GKE-only `gmp-critical`; metadata-server ADC via `credentials.enabled=false`).

---

## Implementation Phases

### Phase 1 — `PROVISION_MODE=self-hosted` with `startupScript`

Everything specified in this proposal: the operator option and validation, `spec.startupScript` with its CEL rules, the self-hosted metadata builder, controller gating (nodepooltemplate, CSR approver), `BootstrapConfigReady`, `id`-only image terms, and manual kOps validation.

### Phase 2 — `spec.userData`

User data via the `user-data` metadata key (additive; mutually exclusive with `startupScript`). Unblocks platforms that consume `user-data` without a shell (e.g. Talos machine config) and distributions that prefer cloud-init delivery. See Alternatives Considered for why `startupScript` ships first.

### Phase 3 — Public-catalog image selection

A `family`/`version` selector grammar against `cos-cloud` and `ubuntu-os-cloud` with defined `latest` semantics, restoring automatic image upgrades in `self-hosted` mode. Until then, phase 1 accepts `id:` selector terms only (see Alternatives Considered).

---

## Alternatives Considered

### Presence-based keying (no flag; `startupScript` set ⇒ self-hosted)

Attractive because it needs no new option and would allow mixing GKE and self-hosted NodeClasses in one deployment. Rejected because the discriminated fact (whether the control plane is GKE) is a property of the deployment, not of a NodeClass (the same reasoning that sources `cluster-name` from `CLUSTER_NAME`). Presence keying also forces awkward mechanics for deployment-scoped components: the nodepooltemplate controller runs on its own timer with no NodeClass in hand, so it would need to list NodeClasses each sync to decide whether calling the Container API is safe, and misconfiguration surfaces per-object at reconcile time instead of at startup. The mixing capability it enables is speculative (no user demand), and AWS's history points the same way: their original presence-based custom-AMI design was replaced by the explicit `amiFamily: Custom` enum. A per-NodeClass mode can still be added later, backward-compatibly, if demand appears.

### Explicit per-NodeClass enum (`amiFamily: Custom` in AWS, `bootstrapMode` in IBM)

More API surface than needed for the deployment-scoped decision, and on GCP there is no per-NodeClass managed/custom split to express today (non-goal). The IBM-style `bootstrapMode: auto|gke|custom` enum remains the natural shape if per-NodeClass mixing is ever wanted.

### Starting with `spec.userData` (cloud-init)

The `user-data` metadata key requires cloud-init on the image and configured for the GCE datasource; `startup-script` is executed by the GCE guest agent on every supported image. Starting with `startupScript` keeps phase 1 image-agnostic and avoids a two-field API (mutual exclusion, two delivery paths) before there is demand for cloud-init user data. `userData` remains a natural additive follow-up.

### Public-catalog `family`/`version` resolution

Considered and deferred: the existing selector grammar and resolvers are GKE-shaped (COS milestone/build versions, `vYYYYMMDD` Ubuntu versions, `-nvda` GPU variants, `latest` scoped to the cluster's Kubernetes patch version), none of which maps cleanly onto `cos-cloud`/`ubuntu-os-cloud`. `id:` pinning covers the provisioner use case (provisioners resolve images themselves and pin exact IDs) at the cost of manual image rollovers (an `id` change still drifts and rolls nodes correctly).


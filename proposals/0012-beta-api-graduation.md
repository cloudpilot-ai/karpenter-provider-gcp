# Proposal: GCENodeClass beta API graduation

- **Status**: Draft
- **Authors**: @dm3ch
- **Created**: 2026-11-16
- **Related Issues**: [Existing work](#existing-work)

## Summary

Graduate `GCENodeClass` to `karpenter.k8s.gcp/v1beta1` with a stable API contract, deliberate deprecation policy, and tested migration. Track the work in a **GCENodeClass v1beta1** milestone; choose its release/date after API review and a migration prototype.

Use graduation to remove deprecated image aliases and the singular pod-range field from beta and canonical provisioning. Prefer a bounded alpha conversion adapter, then retire it under the policy below. This proposal does not implement APIs or create the milestone/issues.

## Motivation

At assessment baseline `401e78cf`, both provider CRDs serve/store only `v1alpha1`. Structured replacements for the deprecated fields already exist. `ImagesReady` already feeds `Ready`; readiness needs a defined contract, not implementation from scratch. Some ownership compatibility protects older live VMs and cannot safely be treated as dead code.

Beta should reduce upgrade surprises and compatibility debt—not become an unbounded feature-parity checklist.

## Proposal

### Scope and guarantees

- Graduate **GCENodeClass**, not the entire provider. Keep `GCECustomMachineType` independently alpha and test its integration with beta NodeClasses.
- Stabilize every retained beta field: name, type, units, defaults, omission/zero/false semantics, validation and documented behavior. Optional does not mean unstable. Redesign or defer unsuitable fields before freeze; do not assume GPU/Linux fields are already gated.
- Initially target GKE Standard, Linux and VPC-native networking. Publish the tested GKE/core, image/architecture and feature matrix; do not inherit an unverified minimum version.
- Exclude provider GA, CNCF status, full AWS parity and unrelated feature requests. Upstream feature gates retain their own maturity.

### Proposed API evolution policy

1. **Compatible extensions stay in the same beta version.** Optional fields and accepted values may be added only if existing configurations, defaults and stored data retain their meaning. Review conversion, enum consumers, list/map topology and server-side apply (SSA); an additive-looking schema diff is not automatically compatible.
2. **Incompatible changes require a successor API.** Field removal/rename, incompatible types, new required fields, changed defaults/meaning or validation rejecting previously valid intended configurations requires a version such as `v1beta2`. Keep the previous version's schema and behavior while it is served. Beta may be superseded by beta or stable, not alpha; stable `v1` needs a separate compatibility policy.
3. **Coexistence must preserve data.** Test bidirectional conversion and read-modify-write through both endpoints. New fields must survive older clients, using reviewed preservation/conversion logic where necessary. Changing the API version does not itself make a migration safe.
4. **Deprecation ships with migration instructions.** Announce the replacement, earliest removal release/date, storage procedure and rollback/recovery boundary. Set CRD deprecation warnings and assign a retirement owner/issue.

**N** is the provider minor release where a usable replacement and the old API's deprecation/migration instructions ship together. Count subsequent provider minor releases—not API versions, Kubernetes releases, patches or prereleases.

| Deprecated API                  | Must remain served in | Earliest removal release | Minimum time after N |
|---------------------------------|-----------------------|--------------------------|----------------------|
| Alpha replaced by beta          | N, N+1                | N+2                      | 90 days              |
| Beta replaced by beta or stable | N, N+1, N+2           | N+3                      | 90 days              |

Removal requires **both minimums plus migration readiness**: verified storage rewriting, supported upgrade-path preflight, conversion and rollback/recovery. Announce delays if these are not ready. A release date never proves that old objects or VMs have migrated. Stop serving versions only in minor releases; if the calendar floor is missed, wait for the next eligible minor, never a patch.

Example: beta introduced in hypothetical `v0.10.0` permits alpha removal no earlier than `v0.12.0`, and only after 90 days and migration gates. If `v1beta2` replaces `v1beta1` in `v0.13.0`, the old beta remains served through `v0.15.x`; `v0.16.0` is its earliest release-count removal point. Monthly cadence can make the calendar floor delay N+2. These are examples, not release targets.

This is proposed **GCP policy**, not Kubernetes core's longer beta support guarantee. A security exception needs explicit maintainer approval, impact analysis and migration/recovery guidance, using private disclosure when necessary; routine bug fixes are not a blanket waiver. Scheduling labels, hashes, Helm values and flags also need compatibility review, but API versioning cannot substitute for their own transition policy.

After acceptance, publish durable compatibility guidance, link it from `CONTRIBUTING.md`, and require compatibility/retirement assessments in API-change reviews. Back enforceable rules with schema/conversion tests. This draft does not silently adopt project-wide rules before agreement.

### Cleanup boundary

| Surface                      | Disposition                                                                                                                                                                                                                                                                                                          |
|------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `imageSelectorTerms[].alias` | Remove from beta/runtime; convert COS aliases to the same family/version and Ubuntu aliases to `Ubuntu2404` with the same version. `latest` must remain `version: latest`, not silently become `channel: cluster`.                                                                                                   |
| `subnetRangeName`            | Remove from beta/runtime; convert to a one-element `subnetRangeNames` list. Test hash/drift effects.                                                                                                                                                                                                                 |
| `imageFamily`                | Review before freeze, especially raw-image-ID bootstrap and mixed-family semantics. It is not already deprecated or approved for deletion.                                                                                                                                                                           |
| Location-label compatibility | Coordinate `belongsToCluster` fallback (`instance.go`), label propagation (`cloudprovider.go`) and the GC guard (`garbagecollection/controller.go`). Require per-target rotation or independently verified ownership backfill before strict matching; retain safeguards otherwise. Continue writing identity labels. |
| Unused compatibility exports | Audit `AnnotationClusterNameTaggedCompatability`, `AnnotationInstanceTagged` and `CompatibilityGroup`, including external Go consumers and persisted metadata meaning, before source removal.                                                                                                                        |

Do not delete upstream-owned legacy repair, useful location defaults, cloud image-deprecation checks or ownership safety tests just because they match “legacy/deprecated.” The literal `drop-me` match is a metadata test fixture, not a production removal marker.

### Migration approach

Prefer same-Kind conversion into one canonical provisioning model; retain deprecated syntax only in the alpha adapter. Conversion serving, certificates, API-server reachability and chart ordering would be new infrastructure and need a prototype before commitment.

Preserve object identities, status and references. `conversion: None` cannot translate renamed fields; changing `storage: true` does not rewrite existing objects. Verify rewriting before retiring `status.storedVersions`. Never delete/recreate active CRDs or rely on an alpha-only controller to read beta storage during rollback.

Test hashes explicitly: normalization must neither trigger accidental fleet rotation nor suppress future genuine drift. Current unequal hash versions skip static drift comparison; bumping the version alone is insufficient. Migrate API objects and old-VM ownership separately.

A hard cutover is an alternative only if maintainers explicitly revise the proposed overlap policy and approve a simpler, fully tested migration/recovery path. AWS's historical Kind renames are not a reason to rename GCENodeClass.

### Milestone backlog

One backlog owns the work below. B1–B10 are release gates; closure requires evidence and maintainer go/no-go, with no unresolved critical authorization, identity-collision, cross-cluster deletion or data-loss defects. Owners remain unassigned. Link existing work without auto-closing it or adopting an old issue's entire scope as a blocker.

#### Existing work

| Tracking                                                                                                          | Required outcome                                                                                                                                                                                                                                   |
|-------------------------------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **B2: [#673 — duplicate VMs after timeout](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/673)**  | Reproduce/fix or demonstrate resolution. Prove one VM per NodeClaim under ambiguous insert outcomes, delayed visibility, cross-zone retries and restart. Existing adoption checks alone are not proof.                                             |
| **B4: [#778 — CSR origin/instance identity](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/778)** | Finish the investigation and record mitigation or explicit beta risk disposition against the [documented trust boundary](../docs/security/csr-approval.md). Do not preselect an attestation protocol or equate documentation with risk acceptance. |
| **B5: [#294 — readiness](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/294)**                    | Reconcile existing ImagesReady/Ready; define actionable dependency failures and recovery at the appropriate status/event layer. Ready cannot guarantee future cloud capacity.                                                                      |
| [#250 — E2E coverage](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/250)                         | Supply targeted B9 scenarios after reconciling the historical checklist; not wholesale AWS parity.                                                                                                                                                 |
| [#755 — PD/Hyperdisk selection](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/755)               | B1 design disposition before freeze; implementation only if deferral preserves a correctness defect or forces a later incompatible change.                                                                                                         |
| [#569 — multiple subnetworks](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/569)                 | B1 design disposition distinguishing subnetworks from pod secondary ranges; feature delivery may follow beta if additive.                                                                                                                          |

#### Proposed new release-gate issues

- [ ] **B1 — Freeze the contract:** per-field/default/validation/maturity review, GKE/kubelet alignment, support matrix, disk/network/image extension decisions and IAM/OAuth-scope policy.
- [ ] **B3 — Prove ownership safety:** old/new VM discovery, adoption, deletion and GC; same-name/different-location isolation; supported per-target preflight. Remove linked fallbacks only when proven safe.
- [ ] **B6 — Choose migration/storage/rollback:** prototype semantic conversion, SSA, metadata/status preservation, interrupted migration, webhook lifecycle and compatible recovery; settle the strategy before dates.
- [ ] **B7 — Implement beta and canonical cleanup:** types/schemes, validation, both chart CRD paths, examples/reference docs, alpha custom-machine-type integration and intentional hash transition.
- [ ] **B8 — Prove populated-cluster upgrades:** legacy/structured inputs, actual storage rewriting, workload/ownership continuity, supported rollback or forward recovery, and accidental/missed drift regressions.
- [ ] **B9 — Collect release evidence:** agree the bounded configuration matrix and workload/scale/soak targets before runs; combine lifecycle, failure/recovery, restart/failover and required hardware evidence.
- [ ] **B10 — Adopt policy and graduate:** durable contributor/operator compatibility rules, enforceable regression checks, versioned migration/support runbook, evidence links and final maintainer decision.

Start B1 alongside safety investigations B2/B3/B4; B6 follows contract decisions, B7 follows B1/B6, and B8/B9 precede B10. These proposed issues have not been created.

#### Proposed cleanup/retirement follow-ups

- [ ] **C1:** remove confirmed unused compatibility exports; optional before beta.
- [ ] **R1:** retire alpha serving/conversion after the approved window and verified migration; assign before beta, but do not block graduation on the window elapsing.
- [ ] **R2:** remove remaining ownership compatibility after per-target prerequisites, if B3 cannot safely retire it before beta.

### Validation

Use current CI plus real API-server tests for schema/defaulting/CEL, conversion round trips, SSA, storage and hash transitions. Exercise ambiguous cloud-create outcomes deterministically below the live-cloud layer.

Validate the advertised image/architecture, on-demand/Spot, private-network and multi-zone baseline without an unnecessary full cross-product. Existing lifecycle/network/storage/image suites are a starting point, not evidence that a candidate passed. Standard E2Es exclude GPU and extended local SSD; advertised hardware-dependent support needs targeted evidence.

A populated-cluster rehearsal must cover alpha → beta, interrupted migration, storage verification, intended disruption under budgets and supported recovery. Validate actual bootstrap, renewal and drain-time renewal separately from offline CSR tests. Injecting a Spot condition does not prove receipt of a GCE notice. Live deployment/tests/cleanup need separate environment-specific approval.

## Precedents

- [AWS beta design](https://github.com/aws/karpenter-provider-aws/blob/9c9ff4acf8fa2b84ee6e4fb7222f5239ea3eba36/designs/v1beta1-api.md): batch justified breaking changes; its non-round-trippable changes motivated new Kinds and coexistence.
- [AWS v1 migration](https://karpenter.sh/v1.0/upgrading/v1-migration/): conversion, compatible rollback releases and persisted-object migration are separate deliverables.
- [Azure API guidance](https://github.com/Azure/karpenter-provider-azure/blob/4dcf32b6cff57c35d7edbdbbe9cae3dde58a502b/designs/0015-nodeclass-vs-labels.md): separate scheduling properties from provisioning configuration and follow managed-platform conventions where appropriate.
- [Kubernetes CRD versioning](https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definition-versioning/) constrains conversion/storage retirement. The [core deprecation policy](https://kubernetes.io/docs/reference/using-api/deprecation-policy/) protects API elements within a version; its beta removal minimum is three Kubernetes minors or nine months after deprecation, whichever is longer—not an automatic guarantee for these CRDs.

## Decisions for review

1. Accept the API-beta scope, per-field contract and alpha N+2 / beta N+3 removal floors, both with 90 days and migration gates?
2. Approve the bounded bridge, or explicitly revise policy for a proven hard cutover?
3. Which retained fields need redesign/deferral, and which disk/network extensions must precede freeze?
4. What bootstrap residual risk, supported configurations and scale/soak targets are acceptable? Assign owners before validation.

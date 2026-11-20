# Proposal: Beta-to-v1 API graduation

- **Status**: Draft
- **Authors**: @dm3ch
- **Created**: 2026-11-16
- **Related Issues**: [Beta backlog](#beta-milestone)

## Summary

Graduate `GCENodeClass/v1beta1` next; track subsequent stable readiness separately. Propose two milestones: **GCENodeClass v1beta1** and **Stable v1 readiness**, with owners/dates assigned after design approval. This proposal creates neither milestones nor APIs.

## Motivation

Production adoption, provider `v1.x`, CRD `v1` and CNCF affiliation are different claims. Beta should establish compatibility and safe upgrades before a stable declaration. The project organization, [GHCR/OCI publishing](../RELEASE.md) and [artifact signing/provenance](../docs/getting-started/installation.md#verify-release-artifacts-optional) already exist; verify candidate releases rather than reopen those migrations.

## Proposal

### Beta scope

Target GKE Standard, Linux and VPC-native networking. Stabilize every retained NodeClass field: names/types/units, defaults, omission/zero/false semantics, validation and behavior. Keep `GCECustomMachineType` independently alpha and test beta integration; upstream feature gates retain their own maturity.

### Audit decisions

Maintain three inventories, with one proposed **Ignore / Fix before beta / Fix before stable** decision per item. Support status is separate: supported, partial, not yet supported, not planned, or owned elsewhere. Ignore does not imply support; not planned is an explicit scope exclusion, revisitable by proposal.

1. **[Missing Terraform support](0012-beta-api-graduation/field-audit.md#missing-support-proposed-delivery-decisions):** prioritize containerd (#788), image-streaming and security/default authority before beta; common kubelet/kernel/logging controls and subnet selection before stable. Other missing features stay explicitly deferred or not planned; full parity is not required.
2. **[Existing names/paths/shapes](0012-beta-api-graduation/field-audit.md#existing-nodeclass-field-alignment):** all 23 current root fields and their children must match Terraform/GKE names, nesting, types/units, cardinality and defaults—or receive an approved Karpenter-specific exception—before beta. Include cloud labels, kubelet/network/GPU/confidential nesting, disk policy ([#755](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/755)) and subnet/range/NIC distinctions ([#569](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/569)).
3. **[Inherited source configuration](0012-beta-api-graduation/source-inheritance.md):** account for 22 raw keys, open kube-env/argument channels, recognized kubelet fields and selected readiness labels. Record current authority, intended defaults/override precedence, omitted/false/empty behavior and effective-config migration. Known breaking transitions and security/default gaps block beta; controls deferred to stable must preserve the frozen beta behavior.

[#788](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/788) is the concrete warning: replacing source-pool containerd settings with cluster defaults plus NodeClass overrides can change existing launches even when the new field is omitted. That is a semantic migration, not automatically an additive API change. The copy-path audit is complete; opaque GKE payloads are explicitly unbounded until their supported configuration contract is closed. No live template values were inspected.

### API evolution policy

- **Compatible additions:** optional fields/values may extend the same beta only while preserving old configurations, defaults and data. Review enum consumers, list/map topology and server-side apply (SSA).
- **Breaking changes:** removal/rename, incompatible types, new required fields, meaning/default changes or rejection of previously valid intended configurations requires a successor such as `v1beta2`. Preserve the older served contract. Beta may be superseded by beta/stable, not alpha.
- **Coexistence:** bidirectional conversion and read-modify-write through either endpoint must preserve information, including newer fields.
- **Deprecation:** ship warnings, an owner/issue, migration instructions, earliest removal release/date and rollback/recovery boundary together.

**N** is the provider minor introducing the usable replacement and deprecation/runbook. Count actual provider minor releases, not patches, prereleases or skipped version numbers.

| Deprecated API     | Served through | Earliest removal | Minimum elapsed time |
|--------------------|----------------|------------------|----------------------|
| Alpha → beta       | N+1            | N+2              | 90 days after N      |
| Beta → beta/stable | N+2            | N+3              | 90 days after N      |

Removal requires both minimums **and verified migration readiness**. Remove only in minor releases; missed calendar/migration gates defer to a later minor, never a patch. Announce delays. These are proposed GCP windows, not Kubernetes core guarantees.

Security exceptions require explicit maintainer approval, impact analysis and recovery guidance—not a generic bug-fix waiver. Unversioned labels, hashes, Helm values and flags need their own transition policy. Publish accepted rules through operator/contributor guidance, link `CONTRIBUTING.md`, and enforce testable rules with regressions. Stable `v1` needs stronger, separately approved guarantees.

### Migration safeguards

Prefer one canonical implementation with a same-Kind alpha conversion adapter. Prototype webhook availability/certificates/reachability and chart ordering before dates; a hard cutover requires explicitly revising the overlap policy.

Preserve identities, status and references. `conversion: None` cannot translate fields; changing storage version does not rewrite objects. Verify rewriting before retiring `status.storedVersions`. Never delete/recreate active CRDs or assume an alpha-only controller can roll back beta storage.

Preserve alias meaning: COS `latest` → `version: latest`, not `channel: cluster`; Ubuntu → Ubuntu2404. Convert singular ranges to singleton lists. Test accidental rotation **and** missed genuine drift; unequal hash versions currently skip static drift checks.

Migrate old VM ownership separately. Coordinate discovery fallback, location propagation and GC guard removal only after per-target rotation/verified backfill; otherwise retain safeguards and identity-label writes. Unused exports are cleanup candidates, not permission to remove upstream repair or current safety checks.

### Beta milestone

B2/B4/B5 use existing issues; B1/B3/B6–B11 need scoped new tracking, with #250 supporting B9. Closure requires evidence, maintainer go/no-go and no unresolved critical authorization, duplicate-identity, cross-cluster deletion or data-loss defects.

| Task                                                                                                   | Completion requirement                                                                                                                                                                                |
|--------------------------------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **B1 — Contract**                                                                                      | Approve all inventory dispositions; deliver only beta-marked outcomes and freeze current fields/approved exceptions. Stable-marked delivery belongs to S2.                                            |
| **B2 — Idempotency ([#673](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/673))**      | Prove one VM per NodeClaim through ambiguous outcomes, delayed visibility, cross-zone retries and restart.                                                                                            |
| **B3 — Ownership**                                                                                     | Test old/new VM adoption/deletion/GC, cluster isolation and migration preflight.                                                                                                                      |
| **B4 — Identity ([#778](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/778))**         | Investigate the [bootstrap trust boundary](../docs/security/csr-approval.md); record mitigation or explicit risk disposition, without preselecting attestation.                                       |
| **B5 — Readiness ([#294](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/294))**        | Build on ImagesReady/Ready; verify actionable dependency failures and recovery.                                                                                                                       |
| **B6 — Migration design**                                                                              | Prove conversion, SSA, preservation, interruption recovery and webhook lifecycle.                                                                                                                     |
| **B7 — Implementation**                                                                                | Resolve B1/B11 blockers; update types, both CRD delivery paths, docs, deprecated paths, hash transition and alpha custom-type integration.                                                            |
| **B8 — Upgrade rehearsal**                                                                             | Populated-cluster rewrite; workload/ownership/effective-config continuity; omitted/disabled/source-change fixtures, recovery and drift regressions.                                                   |
| **B9 — Release evidence ([#250](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/250))** | Agreed configuration/scale/soak, lifecycle/failover/hardware tests and signature/provenance verification—not AWS parity.                                                                              |
| **B10 — Graduation**                                                                                   | Adopt compatibility guidance, regression checks, migration/support runbook and accurate maturity wording; record go/no-go.                                                                            |
| **B11 — Inheritance contract**                                                                         | Close the copy/authority ledger and deliver beta-marked outcomes; bounded inheritance is allowed only where the item's outcome explicitly permits it. Require redacted diffs, migration and rollback. |

B1/B11 and safety investigations start first; B6 precedes B7, B8/B9 precede B10. Reconcile existing issue scope without automatically closing it.

### Stable milestone

**Stable v1 readiness** is a proposed tracking point, not a deadline or full-parity commitment. Decide whether the first stable provider release and CRD `v1` graduate together.

Consider aligning stable provider **major/minor** numbers with the tested `karpenter-core` line, keeping provider patches independent ([AWS](https://github.com/aws/karpenter-provider-aws/blob/v1.15.2/go.mod)/[Azure](https://github.com/Azure/karpenter-provider-azure/blob/v1.15.2/go.mod) examples). The first stable tag need not be `v1.0.0`. Agree GCP-only feature cadence, upgrade lag and backports; publish the exact core dependency and tested compatibility matrix. Matching numbers imply neither feature parity nor CRD stability.

| Proposed task                | Completion requirement                                                                                                                                                            |
|------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **S1 — Beta evidence**       | Agree an observation window; review operator feedback, upgrades, incidents and remaining blockers.                                                                                |
| **S2 — Stable contract**     | Close every Fix before stable item in the feature/inheritance inventories; preserve beta semantics and approve stable support/compatibility guarantees.                           |
| **S3 — Migration**           | Retire eligible alpha adapters; prove beta→v1 conversion/storage/recovery while retaining beta endpoints for their required overlap. Stable need not await that overlap's expiry. |
| **S4 — Production envelope** | Validate supported versions, source-pool changes, WIF/bootstrap/renewal, advertised streaming/hardware and scale/soak; disposition security risks/limitations.                    |
| **S5 — Release decision**    | Approve release numbering/cadence/backports, support/maintenance policy, runbooks, artifact verification and evidence-backed maturity claims. CNCF affiliation is independent.    |

Separate follow-ups: **C1** unused exports (optional before beta); **R1** alpha retirement (assigned before beta, verified in S3); **R2** remaining ownership cleanup after prerequisites. Alpha retirement need not block beta launch. Promote additive features only by explicit scope decision.

### Validation

Require API-server schema/defaulting/CEL/conversion/SSA tests, deterministic ambiguous-create tests and populated-cluster upgrade/recovery. Cover the advertised envelope; standard E2Es exclude GPU/extended SSD. Validate actual bootstrap and drain-time renewal; mocks/injected conditions do not prove real signing or interruption delivery. Live operations need separate approval.

## Review decisions

Approve scope, per-item dispositions (including proposed stable feature additions), overlap policy and migration; assign owners/evidence targets before dates. After approval, split inventory IDs into scoped contribution issues; none are created by this proposal. [AWS's migration experience](https://karpenter.sh/v1.0/upgrading/v1-migration/) and [Kubernetes CRD versioning](https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definition-versioning/) inform the safeguards; pinned field evidence is in the appendix.

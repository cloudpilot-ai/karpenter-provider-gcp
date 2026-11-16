# Proposal: Beta-to-v1 API graduation

- **Status**: Draft
- **Authors**: @dm3ch
- **Created**: 2026-11-16
- **Related Issues**: [Existing work](#existing-work)

## Summary

Target `GCENodeClass/v1beta1` next, with a reviewed compatibility contract, deprecated-field cleanup and tested migration. Propose two GitHub milestones: **GCENodeClass v1beta1** and **Stable v1 readiness**. Leave release numbers/dates unset until the blocking decisions and migration prototype are complete.

This is one graduation roadmap, not a stable-release declaration. The [completed NodeClass/GKE field audit](0012-beta-api-graduation/field-audit.md) informs what belongs before beta, before stable, or in an additive backlog. This PR implements no APIs and creates no milestones/issues.

## Motivation

Production adoption, a provider `v1.0` release, stable `GCENodeClass/v1`, and CNCF affiliation are separate claims. Recommend beta first: prove the API/upgrade contract and resolve foreseeable breaking design gaps; use beta evidence and operator feedback for a later stable decision.

At baseline `401e78cf`, both provider CRDs serve/store only alpha. Structured replacements for deprecated fields and ImagesReady/Ready already exist. Some compatibility protects older live VMs and is not dead code.

Several foundations are already implemented/documented: the separate [project organization](https://github.com/karpenter-gcp), [GHCR/OCI publishing](../RELEASE.md), and [image/chart signing and provenance](../docs/getting-started/installation.md#verify-release-artifacts-optional). Verify the candidate artifacts instead of reopening those migrations. Maturity wording must reflect the tested support matrix, not remove legitimate feature/security warnings.

## Proposal

### Scope and audit decisions

Beta covers **GCENodeClass**, not every provider feature. Keep `GCECustomMachineType` independently alpha and test integration with beta NodeClasses. Every retained beta field needs stable names/types/units, defaults, omission/zero/false semantics, validation and documented behavior; optional does not mean unstable. Initial operating scope is GKE Standard, Linux and VPC-native networking, with an explicit GKE/core/image/architecture/feature matrix.

The source-level audit compares all **47 Terraform Google v8.11.0 NodeConfig root keys**, nested kubelet/Linux settings and relevant networking against actual provider wiring. Repository Terraform examples pin 6.28.0; no dependency upgrade is proposed. “Covered” is not a claim of live validation. The appendix carries the detailed matrix and pinned evidence; these are the pre-beta decisions:

| Audit finding                                                                                                        | Proposed disposition before beta                                                                                                                         |
|----------------------------------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------|
| `spec.labels` means GCE resource labels, unlike GKE/Terraform Kubernetes `labels`                                    | Prefer renaming to `resourceLabels`; Kubernetes labels stay in NodePool metadata. Otherwise explicitly approve the divergence.                           |
| Boot-disk defaults differ from GKE; the general renderer uses configured disks, while swap can materialize a default | Define required boot disk, size/type/cardinality and PD/Hyperdisk policy (#755). Do not imply a universal GKE default exists.                            |
| Kubelet/kernel/GCFS settings may be inherited without NodeClass controls                                             | Define inherited, normalized and rejected settings, especially security defaults and source-pool changes. Inheritance is not declarative feature parity. |
| Private-node inheritance reads the older cluster flag                                                                | Fix/test the newer `NetworkConfig.DefaultEnablePrivateNodes` path, or prove legacy-field mirroring in every supported configuration.                     |
| Confidential-compute shape is flat; extra VM NICs are not extra pod networks or subnet selection                     | Decide existing shapes/semantics now; keep demonstrably additive subnet selection separate (#569).                                                       |
| Kubelet controls deliberately differ from GKE limits/types                                                           | Accept/document or fix GC thresholds, parallel pulls, eviction grace, shutdown and zero/default semantics before freezing them.                          |
| Deprecated aliases/singular range remain                                                                             | Remove from beta/canonical runtime; settle raw-ID/image-family behavior and preserve old meaning during conversion.                                      |

Missing CPU/NUMA/sysctl/logging knobs, PMU, extra swap profiles, attached GPU/MIG, reservations/sole tenancy and advanced networking are **additive candidates**, not automatic beta or v1 blockers. Existing runtime correctness, security and any accepted API-shaping gap are blockers. Full Terraform/GKE feature parity is not a milestone closure condition.

### Proposed API evolution policy

1. **Compatible extensions stay in the same beta version.** Optional fields/accepted values may be added only if old configurations, defaults and stored data retain their meaning. Review conversion, enum consumers, list/map topology and server-side apply (SSA), not just schema additions.
2. **Incompatible changes require a successor API.** Removal/rename, incompatible types, new required fields, changed defaults/meaning or validation rejecting previously valid intended configurations requires a version such as `v1beta2`. Preserve the previous version's schema/behavior while served. Beta may be superseded by beta or stable, not alpha; stable `v1` needs a separately approved compatibility policy.
3. **Coexistence preserves data.** Test bidirectional conversion and read-modify-write through both endpoints. New fields must survive older clients using reviewed preservation/conversion logic. A version bump alone is not a safe migration.
4. **Deprecation includes migration instructions.** Announce replacement, earliest removal release/date, storage procedure and rollback/recovery boundary. Set CRD deprecation warnings and assign a retirement owner/issue.

**N** is the provider minor release where a usable replacement and the old API's deprecation/migration instructions ship together. Patches, prereleases, API version numbers and Kubernetes releases do not advance this clock.

| Deprecated API                  | Must remain served in | Earliest removal release | Minimum time after N |
|---------------------------------|-----------------------|--------------------------|----------------------|
| Alpha replaced by beta          | N, N+1                | N+2                      | 90 days              |
| Beta replaced by beta or stable | N, N+1, N+2           | N+3                      | 90 days              |

Removal requires **both minimums plus verified migration readiness**, not an automatic deadline. Stop serving versions only in minor releases; if calendar/migration gates miss a release, wait for the next eligible minor, never a patch. Monthly cadence can delay alpha retirement beyond N+2. Announce changed targets.

These are proposed GCP windows, not Kubernetes core's longer beta guarantee. A security exception needs explicit maintainer approval, impact analysis and recovery guidance; routine bug fixes are not a blanket waiver. Labels, hashes, Helm values and flags also need compatibility review, but CRD versioning cannot replace their own transition policy.

After acceptance, publish durable operator/contributor guidance, link it from `CONTRIBUTING.md`, require compatibility/retirement assessments in API reviews, and enforce testable rules with schema/conversion regressions. This draft does not silently adopt project-wide rules.

### Migration and cleanup

Prefer same-Kind conversion into one canonical provisioning model, retaining deprecated syntax only in the alpha adapter. Conversion serving/certificates, API-server reachability and chart ordering would be new infrastructure: prototype before committing dates.

Preserve object identities, status and references. `conversion: None` cannot translate renamed fields; changing `storage: true` does not rewrite old objects. Verify storage rewriting before retiring `status.storedVersions`. Never delete/recreate active CRDs or assume an alpha-only controller can roll back beta storage.

Convert COS aliases to the same family/version (`latest` stays `version: latest`, not `channel: cluster`); Ubuntu aliases map to Ubuntu2404. Convert `subnetRangeName` to a singleton list. Test both accidental rotation and missed genuine drift: unequal hash versions currently skip static drift checks, so a version bump alone is insufficient.

Migrate VM ownership separately. Coordinate `belongsToCluster`, cloudprovider location propagation and the GC guard; require per-target rotation or independently verified backfill before strict matching. Retain safeguards otherwise and continue writing identity labels. Audit unused compatibility exports before removal; do not delete upstream repair, useful defaults or current safety checks merely because they say “legacy.”

A hard cutover is an alternative only if maintainers explicitly revise the proposed overlap policy and approve a simpler, fully tested migration/recovery path. AWS's historical Kind renames are not a reason to rename GCENodeClass.

### Milestone 1: GCENodeClass v1beta1

B1–B10 are release gates. Closure requires evidence and maintainer go/no-go, with no unresolved critical authorization, identity-collision, cross-cluster deletion or data-loss defects. Owners remain unassigned. Reconcile existing issue scope; do not automatically close issues or adopt their entire historical scope as a blocker.

#### Existing work

| Tracking                                                                                                                                                                              | Required outcome                                                                                                                                                                                                         |
|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **B2: [#673 — duplicate VMs after timeout](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/673)**                                                                      | Reproduce/fix or prove resolution: one VM per NodeClaim through ambiguous insert outcomes, delayed visibility, cross-zone retries and restart.                                                                           |
| **B4: [#778 — CSR origin/instance identity](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/778)**                                                                     | Finish investigation and record mitigation or explicit risk disposition against the [documented trust boundary](../docs/security/csr-approval.md); do not preselect attestation or equate documentation with acceptance. |
| **B5: [#294 — readiness](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/294)**                                                                                        | Reconcile existing ImagesReady/Ready; define actionable dependency failures/recovery at the correct status/event layer. Ready cannot guarantee future capacity.                                                          |
| [#250 — E2E coverage](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/250)                                                                                             | Supply targeted B9 scenarios, not full AWS parity.                                                                                                                                                                       |
| [#755 — disk selection](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/755), [#569 — subnetworks](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/569) | Resolve the audit's API/default decisions; implement before beta only where deferral preserves a defect or forces an incompatible change.                                                                                |

#### Proposed new issues

- [ ] **B1 — Approve audit dispositions and freeze the contract:** decide the concrete findings above, source inheritance/security policy and per-field support/validation; no generic future gap-audit placeholder.
- [ ] **B3 — Prove ownership safety:** old/new VM adoption, deletion and GC, same-name/different-location isolation and per-target preflight; remove fallbacks only when safe.
- [ ] **B6 — Choose migration/storage/rollback:** prototype semantic conversion, SSA, metadata/status preservation, interruption recovery and webhook lifecycle.
- [ ] **B7 — Implement beta and agreed cleanup:** resolve B1 blockers, remove deprecated canonical paths, update types/schemes, both CRD delivery paths, docs and intentional hash transition; test alpha custom-type integration.
- [ ] **B8 — Prove populated-cluster upgrades:** legacy/structured inputs, storage rewrite, workload/ownership continuity, supported recovery and accidental/missed drift regressions.
- [ ] **B9 — Collect release evidence:** agreed configuration/workload/scale/soak targets; lifecycle, recovery, failover and advertised hardware tests; verify candidate signatures/provenance.
- [ ] **B10 — Adopt policy and graduate:** durable compatibility rules, regression checks, migration/support runbook, accurate maturity wording with retained limitations, and final decision.

Start B1 alongside B2/B3/B4; B6 follows contract decisions, B7 follows B1/B6, and B8/B9 precede B10. New issues have not been created.

### Milestone 2: Stable v1 readiness

Propose this milestone now as the discussion/tracking point, without a release deadline or commitment to deliver every additive feature. Provider `v1.0` and CRD `v1` are separate decisions; the final release decision must state whether they graduate together.

Proposed prerequisites/issues:

- [ ] **S1 — Review beta evidence and feedback:** agree an observation window before scheduling stable; document operator feedback, upgrade/incident results and remaining blockers—not just elapsed time or adoption count.
- [ ] **S2 — Finalize the stable API:** resolve accepted remaining audit/design debt; confirm deferred features can be added compatibly. Approve stronger stable compatibility/support guarantees before creating `v1`.
- [ ] **S3 — Complete migration readiness:** retire eligible alpha adapters under the published gates, test beta→v1 conversion/storage and supported recovery, and preserve deprecated beta endpoints for their required overlap. Stable need not wait for that beta overlap to expire.
- [ ] **S4 — Validate the advertised production envelope:** supported versions/configurations, source-pool changes, WIF/bootstrap/renewal, image streaming where advertised, hardware, lifecycle reliability and agreed scale/soak; explicitly disposition security risks and limitations.
- [ ] **S5 — Make the stable release decision:** approved support/maintenance policy, installation/migration/incident runbooks, repeatable verification of project-owned signed artifacts and maturity wording backed by S1–S4 evidence. CNCF affiliation is independent, not a gate.

Keep feature additions separately prioritized; promote one to S2/S4 only when maintainers accept it as necessary for the stable operating envelope. Do not reopen organization/registry/signing migrations already delivered.

### Cleanup follow-ups

- [ ] **C1:** remove confirmed unused compatibility exports; optional before beta.
- [ ] **R1:** retire alpha serving/conversion under the approved policy; assign before beta, finish when eligible, and verify under S3.
- [ ] **R2:** remove remaining ownership compatibility after per-target prerequisites, if B3 cannot retire it safely.

### Validation

Use current CI plus real API-server tests for schema/defaulting/CEL, conversion round trips, SSA, storage and hashes. Exercise ambiguous creates deterministically below the live-cloud layer. Cover the advertised image/architecture, Spot/on-demand, private-network and multi-zone baseline without an unnecessary full cross-product; standard E2Es exclude GPU/extended SSD, so advertised hardware support needs targeted evidence.

Rehearse populated-cluster migration, interruption, intended disruption under budgets and supported recovery. Validate actual bootstrap/renewal, including drain-time renewal; offline CSR tests and injected Spot conditions are not proof of real signing/notice delivery. Live deployment/tests/cleanup need separate environment-specific approval.

## Precedents

[AWS beta design](https://github.com/aws/karpenter-provider-aws/blob/9c9ff4acf8fa2b84ee6e4fb7222f5239ea3eba36/designs/v1beta1-api.md) supports batching justified breaks; its [v1 migration](https://karpenter.sh/v1.0/upgrading/v1-migration/) separates conversion, storage and rollback. [Kubernetes CRD versioning](https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definition-versioning/) constrains implementation; core's [three-minor/nine-month beta deprecation minimum](https://kubernetes.io/docs/reference/using-api/deprecation-policy/) does not automatically govern these CRDs. The audit supplies pinned GKE/Terraform evidence.

## Decisions for review

1. Accept beta next and the proposed stable-readiness milestone, without conflating production use, release SemVer, CRD stability or CNCF status?
2. Approve the audit's pre-beta shape/default/inheritance decisions and additive deferrals?
3. Accept alpha N+2 / beta N+3 removal floors, both with 90 days and migration gates, and the bounded bridge?
4. Assign owners and agree beta/stable evidence targets, observation window, security-risk disposition and stable compatibility policy before setting dates?

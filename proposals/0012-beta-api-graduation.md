# Proposal: Beta-to-v1 API graduation

- **Status**: Draft
- **Authors**: @dm3ch
- **Created**: 2026-11-16
- **Related Issues**: [Beta backlog](#beta-milestone)

## Summary

Graduate `GCENodeClass/v1beta1` next; track subsequent stable readiness separately. Propose two milestones: **GCENodeClass v1beta1** and **Stable v1 readiness**, with owners/dates assigned after design approval. This proposal creates neither milestones nor APIs.

## Motivation

Production adoption, provider `v1.0`, CRD `v1` and CNCF affiliation are different claims. Beta should establish compatibility and safe upgrades before a stable declaration. The project organization, [GHCR/OCI publishing](../RELEASE.md) and [artifact signing/provenance](../docs/getting-started/installation.md#verify-release-artifacts-optional) already exist; verify candidate releases rather than reopen those migrations.

## Proposal

### Beta scope

Target GKE Standard, Linux and VPC-native networking. Stabilize every retained NodeClass field: names/types/units, defaults, omission/zero/false semantics, validation and behavior. Keep `GCECustomMachineType` independently alpha and test beta integration; upstream feature gates retain their own maturity.

### Audit decisions

The [source audit](0012-beta-api-graduation/field-audit.md) covers 47 Terraform NodeConfig keys plus kubelet/Linux/networking settings. It is not live validation. Resolve these before beta:

| Finding                                               | Decision                                                                                                                                                                    |
|-------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `labels` means GCE resource labels, unlike GKE        | Prefer `resourceLabels`; Kubernetes labels remain in NodePool metadata.                                                                                                     |
| Boot-disk defaults differ from GKE                    | Specify boot cardinality, size/type/defaults and PD/Hyperdisk policy ([#755](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/755)).                          |
| Kubelet/kernel/GCFS settings can be inherited         | Define inherited, normalized and rejected settings, including security defaults and source-pool changes.                                                                    |
| Private-node inheritance reads the older cluster flag | Fix/test `NetworkConfig.DefaultEnablePrivateNodes` or prove legacy-field mirroring throughout the supported matrix.                                                         |
| Confidential/network shapes differ from GKE           | Decide current nesting/semantics; separate subnet selection from pod ranges and extra VM NICs ([#569](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/569)). |
| Kubelet ranges/types differ                           | Approve/document or fix GC thresholds, parallel pulls, eviction/shutdown and zero/default semantics.                                                                        |
| Deprecated fields remain                              | Remove aliases/singular range from beta; settle raw-ID/image-family behavior.                                                                                               |

Full GKE/Terraform parity is not required. Missing knobs are individually reviewed additive candidates; accepted correctness, security and incompatible-shape gaps block graduation.

### API evolution policy

- **Compatible additions:** optional fields/values may extend the same beta only while preserving old configurations, defaults and data. Review enum consumers, list/map topology and server-side apply (SSA).
- **Breaking changes:** removal/rename, incompatible types, new required fields, meaning/default changes or rejection of previously valid intended configurations requires a successor such as `v1beta2`. Preserve the older served contract. Beta may be superseded by beta/stable, not alpha.
- **Coexistence:** bidirectional conversion and read-modify-write through either endpoint must preserve information, including newer fields.
- **Deprecation:** ship warnings, an owner/issue, migration instructions, earliest removal release/date and rollback/recovery boundary together.

**N** is the provider minor introducing the usable replacement and deprecation/runbook. Patches and prereleases do not count.

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

B2/B4/B5 use existing issues; B1/B3/B6–B10 need scoped new tracking, with #250 supporting B9. Closure requires evidence, maintainer go/no-go and no unresolved critical authorization, duplicate-identity, cross-cluster deletion or data-loss defects.

| Task                                                                                                   | Completion requirement                                                                                                                                          |
|--------------------------------------------------------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **B1 — Contract**                                                                                      | Approve audit dispositions, inheritance/security policy and retained-field validation/support.                                                                  |
| **B2 — Idempotency ([#673](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/673))**      | Prove one VM per NodeClaim through ambiguous outcomes, delayed visibility, cross-zone retries and restart.                                                      |
| **B3 — Ownership**                                                                                     | Test old/new VM adoption/deletion/GC, cluster isolation and migration preflight.                                                                                |
| **B4 — Identity ([#778](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/778))**         | Investigate the [bootstrap trust boundary](../docs/security/csr-approval.md); record mitigation or explicit risk disposition, without preselecting attestation. |
| **B5 — Readiness ([#294](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/294))**        | Build on ImagesReady/Ready; verify actionable dependency failures and recovery.                                                                                 |
| **B6 — Migration design**                                                                              | Prove conversion, SSA, preservation, interruption recovery and webhook lifecycle.                                                                               |
| **B7 — Implementation**                                                                                | Resolve B1 blockers; update types, both CRD delivery paths, docs, deprecated paths, hash transition and alpha custom-type integration.                          |
| **B8 — Upgrade rehearsal**                                                                             | Populated-cluster storage rewrite, workload/ownership continuity, recovery and drift regressions.                                                               |
| **B9 — Release evidence ([#250](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/250))** | Agreed configuration/scale/soak, lifecycle/failover/hardware tests and signature/provenance verification—not AWS parity.                                        |
| **B10 — Graduation**                                                                                   | Adopt compatibility guidance, regression checks, migration/support runbook and accurate maturity wording; record go/no-go.                                      |

B1 and safety investigations start first; B6 precedes B7, B8/B9 precede B10. Reconcile existing issue scope without automatically closing it.

### Stable milestone

**Stable v1 readiness** is a proposed tracking point, not a deadline or full-parity commitment. Decide explicitly whether provider `v1.0` and CRD `v1` graduate together.

| Proposed task                | Completion requirement                                                                                                                                                            |
|------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **S1 — Beta evidence**       | Agree an observation window; review operator feedback, upgrades, incidents and remaining blockers.                                                                                |
| **S2 — Stable contract**     | Resolve accepted API/design debt, confirm deferred features' compatibility, and approve stable support/compatibility guarantees.                                                  |
| **S3 — Migration**           | Retire eligible alpha adapters; prove beta→v1 conversion/storage/recovery while retaining beta endpoints for their required overlap. Stable need not await that overlap's expiry. |
| **S4 — Production envelope** | Validate supported versions, source-pool changes, WIF/bootstrap/renewal, advertised streaming/hardware and scale/soak; disposition security risks/limitations.                    |
| **S5 — Release decision**    | Approve maintenance/support policy, operational runbooks, artifact verification and evidence-backed maturity claims. CNCF affiliation is independent.                             |

Separate follow-ups: **C1** unused exports (optional before beta); **R1** alpha retirement (assigned before beta, verified in S3); **R2** remaining ownership cleanup after prerequisites. Alpha retirement need not block beta launch. Promote additive features only by explicit scope decision.

### Validation

Require API-server schema/defaulting/CEL/conversion/SSA tests, deterministic ambiguous-create tests and populated-cluster upgrade/recovery. Cover the advertised envelope; standard E2Es exclude GPU/extended SSD. Validate actual bootstrap and drain-time renewal; mocks/injected conditions do not prove real signing or interruption delivery. Live operations need separate approval.

## Review decisions

Approve scope, audit dispositions, overlap policy and migration approach; assign owners and evidence targets before dates. [AWS's migration experience](https://karpenter.sh/v1.0/upgrading/v1-migration/) and [Kubernetes CRD versioning](https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definition-versioning/) inform the safeguards; pinned field evidence is in the appendix.

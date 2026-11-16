# Proposal: GCENodeClass beta API graduation

- **Status**: Draft
- **Authors**: @dm3ch (draft for review)
- **Created**: 2026-11-16
- **Related Issues**: [#673](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/673), [#294](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/294), [#778](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/778), [#250](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/250)

## Summary

Graduate `GCENodeClass` from `karpenter.k8s.gcp/v1alpha1` to `karpenter.k8s.gcp/v1beta1`, using a GitHub milestone to track a finite set of API, migration, security, and lifecycle release gates. Beta means that operators can rely on the retained API and have a tested upgrade path. It does not mean implementing every GCP feature, matching every AWS test, graduating all upstream feature gates, or declaring the entire provider generally available.

Use graduation to remove the deprecated `imageSelectorTerms[].alias` and `subnetRangeName` fields from the beta schema and canonical provisioning path. Prefer a time-bounded alpha compatibility adapter over duplicating old provisioning implementations. Some other compatibility paths can disappear at the same time; paths protecting older live instances need their own migration prerequisites. This proposal compares a compatibility bridge with a breaking cutover and leaves the final choice to maintainers.

## Motivation

### Problem statement

The current APIs have accumulated both deliberate compatibility and unfinished design decisions. Changing only the version string would neither settle those decisions nor make upgrades safe. Conversely, treating every feature request and TODO as a graduation blocker would make beta unbounded.

At the assessment baseline, `401e78cf` on `main`:

- Both `GCENodeClass` and `GCECustomMachineType` serve and store only `v1alpha1`.
- `GCENodeClass` explicitly deprecates the image alias and singular pod-range fields. The structured image selectors and plural pod-range replacement already exist.
- `Ready` aggregates `ImagesReady`; readiness is not absent, but it does not describe every bootstrap, network, or identity dependency.
- Three linked ownership TODOs allow discovery of older location-label-less VMs while preventing their automatic orphan collection. Removing just one can hide live nodes or weaken deletion safety.
- Some current public issue descriptions predate substantial implementation. Issue state alone is not evidence that a requirement is unmet.

### Goals

- Agree what beta guarantees before choosing a release date.
- Batch justified breaking API cleanup into one reviewed transition.
- Maintain one canonical provisioning implementation, with temporary compatibility at the boundary where necessary.
- Define objective release gates and a small, dependency-ordered milestone backlog.
- Make storage migration, existing-node behavior, rollback, and eventual compatibility removal explicit.

### Non-goals

- A provider `v1.0` release, CNCF acceptance, or a managed-GKE support commitment.
- Graduating `GCECustomMachineType`; it remains `v1alpha1` unless separately reviewed.
- Reserved capacity, self-hosted Kubernetes, Windows, full AWS feature/test parity, or all open feature requests.
- Deleting upstream-owned implementations from `vendor/`.
- Deploying controllers, migrating clusters, running live tests, or creating GitHub resources as part of accepting this design document.

## Proposal

### 1. Define the graduation contract

The proposed milestone is **GCENodeClass v1beta1**. Leave its due date and release number unset until the API review and migration prototype establish the work remaining.

Every field retained in `v1beta1` is part of the beta contract: names, types, units, requiredness, defaulting, omitted versus explicit zero/false behavior, and documented effects. A statement that an optional feature is experimental is not permission to make incompatible changes to its beta fields. Before freeze, either stabilize that surface or explicitly defer it from beta with an alpha-only design. Do not assume that GPU, swap, or hugepage fields are feature-gated today.

Within this API version, avoid field removal/renames, new required fields, silent semantic/default changes, or tighter validation that rejects previously valid intended configurations. Prefer reviewed additive optional changes. Security and correctness fixes may require exceptions, but need explicit migration/release guidance rather than an implied compatibility waiver. Define changes to scheduling labels, hash annotations, Helm values, flags, and defaults in the same release policy: those are operator contracts even when they are not CRD fields.

The initial supported operating envelope should be GKE Standard, Linux, and VPC-native networking. Publish an actually tested GKE/Kubernetes and karpenter-core compatibility matrix instead of carrying forward an unverified minimum version. Document supported architecture/image combinations and the maturity of opt-in upstream features independently. Keeping `GCECustomMachineType` alpha must not require an alpha NodeClass to use it; test catalog integration with beta NodeClasses.

### 2. Finish shape decisions before adding more features

Review the entire retained NodeClass surface once, against current GKE NodeConfig conventions where equivalent and the upstream kubelet schema for kubelet settings. Record intentional differences rather than mechanically renaming everything. Prioritize decisions whose deferral would force another incompatible change:

| Area                     | Decision needed before freeze                                                                                                                                                    | Implementation needed before beta?                                                                                  |
|--------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------|
| Images                   | Remove alias; define `imageFamily` precedence, raw-ID bootstrap family, mixed-family terms, and `latest` versus channel semantics.                                               | Yes for unambiguous retained behavior; top-level `imageFamily` is a review candidate, not already deprecated.       |
| Pod ranges / subnetworks | Remove singular range; distinguish several secondary ranges on one subnet from several subnetworks ([#569](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/569)). | Multi-subnetwork selection can wait if it is additive; the distinction and current limits cannot.                   |
| Disks                    | Resolve naming/defaults and extension path for PD/Hyperdisk selection ([#755](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/755)).                              | Selection policy can wait if additive; unsafe or misleading existing behavior cannot.                               |
| Identity                 | Define service-account fallback, IAM prerequisites, OAuth scope policy, and bootstrap trust assumptions.                                                                         | A secure, documented supported configuration is required; a new scopes API is not automatically required.           |
| Kubelet / Linux / GPU    | Settle units, zero/false semantics, unsupported combinations, scheduling capacity versus node runtime, and field maturity.                                                       | Every retained field needs a defensible contract; new hardware features are not required.                           |
| Status and drift         | Define what Ready proves, actionable failures, observed-generation handling, and which changes replace nodes.                                                                    | Yes for retained behavior; do not promise synchronous validation of every GCP permission or future cloud operation. |

Do not add speculative fields merely to reserve space. A recorded demonstration that an extension is additive is a valid outcome; "implement every request before beta" is not.

### 3. Cleanup and removal inventory

| Item and current source                                                                                                                                                                                   | Proposed disposition                                                                                                                                          | Safety prerequisite                                                                                                                                                                                                                                              |
|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `imageSelectorTerms[].alias`; `Alias()`/parsing in `pkg/apis/v1alpha1/gcenodeclass.go`, alias resolver in `pkg/providers/imagefamily/image.go`                                                            | Remove from beta and canonical runtime; keep only boundary conversion while alpha is served.                                                                  | Map COS aliases to the same family/version and Ubuntu aliases to `Ubuntu2404` with the same version. Preserve existing selection semantics.                                                                                                                      |
| `subnetRangeName`; `PodSubnetRangeNames()` fallback                                                                                                                                                       | Remove from beta and canonical runtime; convert to a one-element `subnetRangeNames` list.                                                                     | Test persisted-object conversion and hash/drift effects; existing migration notes already warn of drift.                                                                                                                                                         |
| `imageFamily`                                                                                                                                                                                             | Decide retain, constrain, or replace during API review; not an approved deletion.                                                                             | Account for exact-image-ID users and OS-specific bootstrap; do not remove it merely because structured selectors can infer some families.                                                                                                                        |
| `belongsToCluster()` fallback in `pkg/providers/instance/instance.go`, location propagation in `pkg/cloudprovider/cloudprovider.go`, guard in `pkg/controllers/nodeclaim/garbagecollection/controller.go` | One coordinated cleanup, conditional on ownership migration. Keep protections if the prerequisite is not met; continue writing identity labels after cleanup. | A supported upgrade-path requirement and per-target preflight/rotation or independently verified backfill procedure for older VMs, strict identity filtering, and same-name/different-location isolation tests. Never infer fleet migration from a release date. |
| `AnnotationClusterNameTaggedCompatability`, `AnnotationInstanceTagged`, and possibly `CompatibilityGroup`                                                                                                 | Candidate source cleanup: only declarations were found in tracked provider source at this baseline.                                                           | Confirm generated/downstream Go usage and whether persisted annotations still have a documented meaning; do not confuse an unused constant with permission to delete external metadata.                                                                          |
| `legacyNodeRepair` chart exposure                                                                                                                                                                         | Decide deprecation policy separately; modern repair is the default.                                                                                           | Implementation and flag belong to karpenter-core. Removing a Helm value alone does not remove that code path; follow upstream lifecycle and validate any provider opt-out.                                                                                       |
| `NodeLocation` fallback to `ClusterLocation`                                                                                                                                                              | Retain unless a specific migration benefit justifies removal.                                                                                                 | This is a useful default, not proof of obsolete behavior.                                                                                                                                                                                                        |
| OAuth-scope TODO and `GetInstanceTypes` coupling TODO                                                                                                                                                     | Make an explicit security/design disposition; otherwise ordinary follow-up work.                                                                              | IAM/scope analysis for the first; a concrete benefit and preserved behavior for the second. Neither is a mechanical delete.                                                                                                                                      |
| E2E `--preset` / `E2E_PRESET` aliases                                                                                                                                                                     | Optional small tooling cleanup, not a beta blocker.                                                                                                           | Update callers and tooling documentation; do not conflate test CLI compatibility with the public NodeClass API.                                                                                                                                                  |
| Cloud image `DEPRECATED` checks, `disable-legacy-endpoints`, older-instance safety tests                                                                                                                  | Retain.                                                                                                                                                       | These implement present-day safety, not deprecated provider APIs.                                                                                                                                                                                                |

No production literal `drop-me` marker was found in the tracked-source scan; the match in `pkg/metadata/compute_metadata_test.go` is a test value demonstrating that unknown source metadata is dropped. `context.TODO()` is also not a removal instruction. Keep historical migration instructions/proposals and tests for every compatibility path still supported.

### 4. Choose a migration strategy deliberately

#### Recommended: bounded compatibility bridge

Keep the group, Kind, resource names, and object identities. Introduce beta alongside alpha, using conversion when representations differ. Alpha input is normalized into the canonical model before ordinary provider logic; beta must not reintroduce deprecated fields. This allows deletion of duplicate legacy provisioning logic without promising immediate deletion of every alpha type or adapter.

The prototype must prove:

1. Semantic alpha → beta → alpha round trips for both old deprecated input and existing structured input; preservation of metadata, status, UIDs, references, and effective launch configuration.
2. Preservation of COS `latest` as `version: latest`, not an automatic switch to `channel: cluster`; the latter is a policy change even if recommended for new users. Ubuntu's old alias selects the Ubuntu 24.04 resolver today.
3. A defined mapping for every beta value while alpha remains served. If an alpha client cannot represent a new beta value, redesign the overlap or explicitly constrain it; do not silently lose data.
4. Correct CRD installation order, conversion availability/certificates/RBAC and failure behavior, storage rewriting, server-side apply/field ownership, and one documented rollback target that understands the stored data.
5. An intentional hash-version strategy. Syntax-only normalization must not accidentally cause uncontrolled fleet rotation or suppress later genuine drift. Current static drift checks skip unequal hash versions; a version bump alone is not a complete transition policy. If replacement is necessary, announce it and demonstrate disruption controls.

The provider does not currently expose conversion-webhook infrastructure in its controller/charts. A bridge adds an availability and certificate-management obligation, not just a generated type. Unknown fields can be pruned rather than reliably rejected on every client path; preflight and strict validation guidance must prevent silent loss.

Changing `storage: true` does not rewrite existing objects. Removing an old entry from `status.storedVersions` is the last bookkeeping step after verified migration, not a substitute for it. `conversion: None` cannot rename the singular range or translate aliases. Helm chart/controller and CRD upgrade paths must be documented separately; upgrading only the controller is insufficient.

Proposed retirement policy for review: keep alpha serving for **at least two subsequent minor releases and at least 90 days after first beta**, announce removal at introduction, and remove it only after migration and rollback criteria pass. These numbers are a proposed GCP policy, not an upstream requirement. The beta milestone can close with this bounded adapter present; alpha retirement is a separately tracked follow-up with an owner and earliest eligible release. The bridge must remain deployable on the supported controller placement, including any supported network restrictions.

#### Alternative: explicitly breaking cutover

A beta-only release can provide a smaller steady-state implementation, but transfers risk to operators and needs stronger preflight and migration tooling. It is acceptable only if maintainers explicitly choose the break and prove a supported sequence that:

- transforms all manifests, existing stored NodeClasses, and supported client/GitOps integrations before removing alpha access;
- preserves resource identity and NodeClaim/NodePool relationships;
- proves the storage transition and a recovery plan for interrupted migration;
- accounts for old VMs independently of API-object migration;
- publishes a maintenance window, rollback boundary, and unsupported skip-upgrade paths.

This may still need a transitional converter or bridge release: a hard compatibility policy is not evidence that conversion engineering disappears. Never implement the cutover by deleting/recreating an active CRD, pruning unknown fields and hoping for equivalent defaults, or changing only `apiVersion`. Do not rely on an alpha-only controller as a rollback target after beta storage exists.

**Recommendation:** prototype the bounded bridge first. Reconsider a hard cutover only if its complete tested path is materially simpler and its disruption is explicitly accepted. Do not rename the Kind merely to copy AWS's historical migration; that rename addressed its particular non-round-trippable API changes.

### 5. GitHub milestone and release gates

Create the milestone only after this proposal is accepted and publication is separately authorized. Assign an owner to each child issue, link the proposal and evidence in issue bodies, and reuse existing issues after reconciling their descriptions with current implementation. Do not close stale issues automatically.

The following gates are blocking; feature parity is not:

| Gate                               | Required completion evidence                                                                                                                                                                                                                                                                                                                                                                                                |
|------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| API contract                       | Reviewed field/default/validation/maturity inventory; deprecated surface absent from beta; additive extension decisions recorded; generated CRDs, schema tests, examples and reference docs agree.                                                                                                                                                                                                                          |
| Creation idempotency               | Resolve or disprove [#673](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/673) on current code. Tests cover an insert whose outcome is unknown after timeout, delayed visibility, retry in another zone, and controller restart: at most one managed VM per NodeClaim. Existing adoption checks are not alone proof.                                                                                        |
| Ownership and deletion             | Proven discover/adopt/delete/GC behavior for supported old and new nodes; same-named clusters in different locations cannot collect each other's VMs. Any deferred strict-label cleanup has an owner and prerequisite, not a removed guard.                                                                                                                                                                                 |
| Bootstrap and certificate security | Supported bootstrap and renewal, including renewal during drain, validated separately from offline CSR tests. Assess [#778](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/778) and the documented shared-bootstrap trust boundary; record mitigation or explicit maintainer risk acceptance for the advertised beta envelope. Known critical authorization flaws cannot be waived by calling the API beta. |
| Readiness and diagnosis            | Reconcile [#294](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/294) against existing ImagesReady. Missing/unusable images, source-pool/bootstrap failure, bad network configuration and identity failures have actionable status/events and recovery behavior at their appropriate layer; stale status does not authorize unsafe new launches.                                                             |
| Migration and rollback             | Representative populated-cluster upgrade and supported rollback/recovery rehearsals, storage rewrite verification, interrupted-conversion failure tests, GitOps/SSA validation, and predictable hash/drift outcomes.                                                                                                                                                                                                        |
| Operational baseline               | Passing supported lifecycle tests, controller restart/failover and cloud-error recovery checks, plus a recorded soak/scale result against a workload and target agreed before the run. No unresolved critical data-loss, identity-collision, cross-cluster deletion, or credential-exposure bugs.                                                                                                                           |
| Release and support contract       | Version/feature matrix, migration/removal policy, installation/CRD ordering, limitations, diagnostics, security reporting, signed artifacts and release checks aligned with the chosen beta release. Final maintainer go/no-go references the evidence.                                                                                                                                                                     |

Single-VM safety, ownership, and bootstrap authorization come before adding more configurable surface. API-shape decisions come before freezing the schema. Migration/storage correctness comes before removing alpha serving. Repeatable release evidence comes before declaring graduation.

### 6. Proposed milestone backlog

Use **GCENodeClass v1beta1** as the milestone name, with no release number or due date until the contract review and migration prototype are complete. The following is a proposed backlog, not a claim that issues have already been created or assigned.

#### Existing issues to address or reconcile

- [#673 — Duplicate VMs after operation timeout](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/673): highest-priority lifecycle gate. Reproduce and fix, or demonstrate that current code already satisfies the single-VM invariant, with timeout, delayed-visibility, cross-zone and restart regression evidence.
- [#778 — Verify node CSR origin against GCE instance identity](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/778): complete the identity investigation and make an explicit beta security decision. Do not assume a particular attestation implementation is already required or available.
- [#294 — GCENodeClass readiness conditions](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/294): reconcile the request with existing ImagesReady/Ready, then finish the agreed dependency diagnostics and recovery contract.
- [#250 — E2E coverage](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/250): reconcile its historical checklist and select the lifecycle, failure/recovery and upgrade scenarios needed for the beta support matrix; full AWS parity is not a gate.
- [#755 — PD/Hyperdisk boot-disk selection](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/755): decide the API extension path before freeze. Implement now only if deferral would preserve a correctness problem or require another incompatible change.
- [#569 — Multiple subnetworks / subnetwork selectors](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/569): settle the distinction from multiple pod ranges and the extension path. Feature implementation can follow beta if it is additive.

These are related work, not issues this proposal-only PR fixes or closes. Reconcile acceptance criteria before assigning existing issues to the milestone; do not make a broad historical issue's entire scope a blocker by accident.

#### Proposed new release-gate tasks — issues not yet created

- [ ] **B1 — Freeze the beta API contract:** review every retained field, defaults, validation, maturity, drift and supported environments; record the disk/network/image decisions above.
- [ ] **B3 — Prove ownership-safe upgrades and garbage collection:** cover old/new VMs and foreign-cluster isolation, define per-target preflight, and either demonstrate coordinated fallback removal is safe or retain it with a follow-up.
- [ ] **B6 — Prototype and choose migration/storage/rollback:** prove semantic conversion, storage rewrite, object identity, SSA and recovery; compare the complete bridge and hard-cutover paths before choosing.
- [ ] **B7 — Introduce v1beta1 and remove deprecated canonical paths:** update types, schemes, both chart CRD paths, validation, examples and reference docs; test integration with alpha GCECustomMachineType.
- [ ] **B8 — Add populated-cluster upgrade and drift regression coverage:** exercise legacy/structured inputs, interrupted migration, supported rollback or recovery, and both unintended rotation and missed genuine drift.
- [ ] **B9 — Establish beta release evidence:** define the bounded version/configuration matrix and scale/soak targets, then collect lifecycle, restart/failover, bootstrap/renewal and required hardware evidence. Reuse targeted #250 work rather than duplicate it.
- [ ] **B10 — Complete the operator contract and graduation decision:** publish the tested migration/support/removal policy and assemble all release-gate evidence for maintainer go/no-go.

The missing numbers refer to existing tracking: **B2 = #673**, **B4 = the #778 investigation plus beta risk disposition**, and **B5 = #294**. This gives ten bounded release-gate items; B9 draws on #250 rather than adopting its full parity goal. Owners remain unassigned. Start B1 alongside the B2/B3/B4 safety investigations; B6 follows the contract decisions, B7 follows B1/B6, and B8/B9 precede B10.

#### Proposed cleanup and retirement follow-ups — issues not yet created

- [ ] **C1 — Remove confirmed unused compatibility exports:** audit external consumers and persisted metadata meaning before deleting the candidate constants. Optional before beta, not a graduation blocker.
- [ ] **R1 — Retire alpha serving and conversion:** if a bridge is chosen, give this a separate owner and the approved minimum window, storage-migration and recovery prerequisites before shipping beta. It need not complete for the beta milestone to close.
- [ ] **R2 — Tighten ownership filtering and remove redundant GC compatibility:** only if this cannot safely complete under B3; require the supported upgrade-path/per-target checks and continue writing identity labels.

## Test plan

### Offline and unit/integration tests

Use focused tests first, then the applicable existing checks: `make ut-test`, `make verify`, `make update`, `make verify-codegen`, `make verify-crds`, `make chart-lint`, and `make docs-lint`. Generated diffs must be distinguished from expected source changes. Include real API-server schema/conversion tests, not only fake-client tests, for defaulting, CEL, storage, list/map topology and SSA behavior.

Cover omitted versus explicit values, invalid field combinations, semantic conversion of deprecated fields, beta writes read by alpha clients during overlap, conversion unavailability, status preservation, and hash/drift stability. Exercise ambiguous cloud-create outcomes deterministically below the live-cloud layer.

### Live validation to authorize separately

Build on existing `provisioning`, `drift`, `consolidation`, `expiration`, `gc`, `repair`, `interruption`, `networking`, `storage`, and `channel-image-selection` suites; their presence is not proof that a beta candidate passed them. Reconcile the old checklist in #250 rather than requiring wholesale AWS parity.

Validate a compact matrix spanning the advertised image/architecture, on-demand/Spot, private-network, and multi-zone baseline without requiring every cross-product. Include full launch → registration → workload → disruption → deletion, loss of template pool and recovery, IP exhaustion, image unavailability, and restart/failover. Require targeted hardware validation for hardware-dependent behavior retained and advertised as supported; standard tests exclude GPU and extended local SSD.

Add a populated-cluster upgrade scenario: launch from the supported alpha release using old and structured selectors, upgrade using the approved sequence, confirm ownership and workload continuity, migrate storage, exercise intended drift under budgets, and rehearse the supported rollback or documented forward recovery. Bootstrap/renewal validation must cover real signing and GKE join/rotation; a condition-injection interruption test is not proof of receiving a real GCE notice. Every run needs its own explicit environment/operation approval; this proposal authorizes none.

## Acceptance criteria

- [ ] Maintainers approve the graduation boundary, every retained field's contract, and the migration option.
- [ ] The milestone has a finite blocker list, owners, evidence requirements, and explicitly deferred work.
- [ ] Beta schema and canonical runtime have no deprecated alias/singular-range behavior; any remaining alpha adapter has a retirement issue and policy.
- [ ] All release gates above have evidence and no unresolved blocking defects.
- [ ] Any removed ownership compatibility has a verified old-node migration prerequisite and coordinated safety tests; otherwise protections remain with a tracked follow-up.
- [ ] Migration, storage, rollback/recovery, hash/drift, and GitOps behavior are documented and tested.
- [ ] The support/maturity matrix and release artifacts match the tested candidate.
- [ ] Maintainers record a final go/no-go; no implied publication or deployment authority is granted by this checklist.

## Implementation phases

1. **Decide:** reconcile current issues, audit the API and cleanup inventory, settle the beta contract and migration option. Prototype conversion and hash behavior before committing to dates.
2. **Harden and normalize:** close lifecycle/security/readiness blockers; introduce a canonical model and necessary preflight or ownership-migration support. Remove confirmed dead provider code in small PRs.
3. **Introduce beta:** generate beta artifacts, boundary compatibility, storage migration, release documentation and upgrade tests; keep experiments out of the stable contract unless explicitly designed.
4. **Validate and graduate:** obtain separately authorized live evidence, run the agreed soak/scale target, and make the release decision.
5. **Retire compatibility:** after the chosen overlap and migration gates, remove alpha serving/conversion and remaining eligible old-node paths. Do not extend the beta milestone indefinitely to absorb this follow-up.

## Migration

This document defines migration requirements, not executable operator instructions. The selected implementation must supply a versioned runbook with exact commands and preflight checks before beta can ship. It must cover manifests, stored resources, controller/CRD ordering, conversion infrastructure if used, old VM identity labels, disruption impact, backup/recovery, supported skip paths, and the rollback boundary. API-object migration and VM rotation/backfill are separate operations and require separate verification.

## Risks and mitigations

| Risk                                                 | Mitigation                                                                                                                                   |
|------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------|
| "Beta" promises more than has been tested            | Bind every retained field to a stable contract; publish the actual operating envelope and evidence.                                          |
| Cleanup destroys compatibility before fleets migrate | Separate beta schema/runtime cleanup from adapter and old-node retirement.                                                                   |
| Conversion becomes a permanent subsystem             | Give it an owner, removal criteria and a stated minimum overlap; keep provider policy out of the adapter.                                    |
| Migration causes unexpected fleet replacement        | Canonicalize and test hashes; announce intentional replacements and validate disruption controls.                                            |
| Backlog grows into feature parity                    | Require a correctness, security, migration, or incompatible-shape reason for each blocker.                                                   |
| Public tracking overstates security guarantees       | Use the existing CSR security contract, separate offline/live evidence, and keep private security findings in the private reporting process. |

## Alternatives considered

- **Only rename the API version:** rejected; it does not establish a compatibility contract or migrate stored data.
- **Provider-wide beta only after feature parity:** rejected; it is unbounded and conflates core feature gates with the provider API.
- **Keep every deprecated field until v1:** rejected; it wastes the deliberate pre-beta API design boundary. A temporary adapter is narrower than retaining deprecated beta fields and runtime branches.
- **Remove every TODO/legacy match:** rejected; several matches are current safety mechanisms, upstream code, or useful defaults.
- **Rename the Kind for side-by-side resources:** fallback only if semantics cannot be safely converted and an independently justified migration design demonstrates the benefit.

## Experience from other Karpenter projects

- **AWS beta (v0.32):** its [API design](https://github.com/aws/karpenter-provider-aws/blob/9c9ff4acf8fa2b84ee6e4fb7222f5239ea3eba36/designs/v1beta1-api.md) explicitly batches breaking changes to reduce migration burden. Its non-round-trippable changes motivated new Kinds and side-by-side resources. The [migration guide](https://karpenter.sh/v1.0/upgrading/v1beta1-migration/) documents tooling and staged node rollover. Lesson: batch justified cleanup, but design the migration rather than assuming API versioning solves it.
- **AWS beta → v1:** the [v1 migration guide](https://karpenter.sh/v1.0/upgrading/v1-migration/) documents conversion webhooks, compatible rollback releases, temporary compatibility annotations, and storage migration before later version removal. Lesson: manifest conversion, persisted-object migration, and rollback are different deliverables.
- **Azure:** at [4dcf32b6](https://github.com/Azure/karpenter-provider-azure/blob/4dcf32b6cff57c35d7edbdbbe9cae3dde58a502b/pkg/apis/crds/karpenter.azure.com_aksnodeclasses.yaml), `AKSNodeClass/v1beta1` is served/storage and deprecated `v1alpha2` remains declared but is not served/storage. This is a current-state observation, not proof of its original graduation process. Its [NodeClass-versus-label guidance](https://github.com/Azure/karpenter-provider-azure/blob/4dcf32b6cff57c35d7edbdbbe9cae3dde58a502b/designs/0015-nodeclass-vs-labels.md) separates scheduling decisions from structured provisioning configuration and uses managed-platform conventions as precedent. Lesson: be explicit about both the API boundary and the retirement state.
- **Kubernetes CRD versioning:** the [versioning guide](https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definition-versioning/) explains why `None` is unsuitable when data moves between fields and why old stored versions must actually be migrated before removal. These are implementation constraints, not optional beta policy.

## Open questions

1. Accept API beta with the operational gates above, independently of provider GA and other alpha CRDs?
2. Choose the bounded bridge or a tested, explicitly breaking cutover? If bridge, approve or change the proposed two-subsequent-minors/90-days minimum.
3. Which optional field surfaces need redesign or deferral rather than retention in beta, especially `imageFamily` and newly added Linux/GPU features?
4. Which shape-affecting disk/network extensions must be implemented now, and which are demonstrably additive?
5. What bootstrap threat model and residual-risk decision is acceptable for beta, given the documented shared-bootstrap boundary and #778?
6. Which GKE versions/configurations, workload scale, and soak duration form the release evidence target? Assign these before validation, not after observing results.
7. Which older-node compatibility can be safely retired at first beta, and which must outlive it?

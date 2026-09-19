# Proposal: On-Demand E2E CI for Pull Requests

- **Status**: Draft
- **Authors**: @dm3ch
- **Created**: 2026-06-21
- **Related Issues**: [#296](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/296), [#250](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/250)

## Summary

Provide maintainer-triggered GKE validation using the sponsored e2e project and a persistent cluster. Deliver the ChatOps/security harness first in [#510](https://github.com/cloudpilot-ai/karpenter-provider-gcp/pull/510), reusable local tooling in PR 2a, then dependent pipeline integration in PR 2b. A separate GCP-only Terraform prerequisite PR supplies CI federation and runtime identity before cloud activation.

**#510 validates the harness, not real e2e suites.** Its bootstrap runner advertises standard mode as `not_implemented`; a successful harness check accompanies a neutral real-e2e check. GPU/full execution is also deferred. Provisioning, maintenance and teardown are never normal PR-run operations.

## Goals and Non-Goals

Goals:

- Maintainer authorization before executing immutable PR code.
- Separate trusted orchestration and code-under-test identities.
- Dedicated runtime credentials without maintenance authority or GitHub writes.
- Reusable local tooling, deterministic quota-aware parallelism and ownership-safe cleanup.
- Tested-SHA checks, sticky comments and retained structured evidence.
- Coverage growth toward AWS-provider parity using [#250](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/250).

Non-goals for this series:

- Automatic execution on every push, GPU/full implementation or a new test framework.
- A general workflow engine, deep request queue, timing service or automatic teardown.
- Treating approved arbitrary PR code as sandboxed.
- Migrating existing cluster provisioning to Terraform before the complete e2e flow works.

## User Interface

| Command         | Mode       | Bootstrap behavior                                   |
|-----------------|------------|------------------------------------------------------|
| `/e2e`          | `standard` | Capability preflight; neutral not implemented        |
| `/e2e standard` | `standard` | Same as bare command                                 |
| `/e2e gpu`      | `gpu`      | Not implemented; no tooling resolution, lock or auth |
| `/e2e full`     | `full`     | Not implemented; no tooling resolution, lock or auth |

Pinned `github/command` verifies `write`, `maintain` or `admin` permission. Exact option validation rejects extra arguments, whitespace, multiline commands and normal issues before execution queueing. Bare `/e2e` still has a human-review-to-SHA-resolution race: authorization approves the PR head captured by the gate, not necessarily the commit the maintainer last viewed.

## Bootstrap Trust and Source Identity

An administrator configures `E2E_TOOLING_REF` as a mutable branch in fixed repository `cloudpilot-ai/karpenter-provider-gcp`, initially proposed as `e2e-ci-bootstrap`. The maintainer must create/review that branch; absence is a failure, not permission to fall back to a fork or PR source.

Each standard invocation resolves exactly `refs/heads/<configured branch>` once through the API. The gate freezes tooling repository/ref/full SHA independently of the tested PR repository/full SHA, together with contract version, triggering repository/PR, run/attempt and runtime configuration. Updating the branch changes subsequent invocations, not an existing invocation. No per-version SHA promotion is needed. Keep the branch through bootstrap; do not automatically delete it after a tooling PR merges.

Write access to this upstream branch grants trusted execution, including reporting. Upstream location alone does not sanitize its contents. All consuming jobs use the captured revision. The workflow bridge comes from the workflow snapshot; trusted CLI binaries are built freshly from the captured tooling checkout, never obtained from a PR-build artifact.

The root-module CLI under `hack/ci/e2e-runner/` exposes versioned `capabilities`, `prepare`, `run` and `report` commands. A read-only preflight must validate the contract before building PR code, locking a target or authenticating. Missing, malformed and incompatible contracts fail closed. Valid unsupported or administratively disabled capability is neutral. The bootstrap contains no scheduler or cloud suite implementation.

The prepared bundle contains code under test: matching PR controller, charts and test binaries, not trusted orchestration. Same-run artifact IDs, separately recorded manifest digests, exact invocation identity and file digests bind handoffs. Reject unsafe paths, symlinks, undeclared files and oversized/malformed JSON. Reporter accepts bounded result data only; uploaded fields cannot choose the reporting PR or SHA. See the [developer contract](../hack/ci/README.md) for current schemas and limits.

PR 2b removes `E2E_TOOLING_REF` and its branch-override path **in the same change** that adopts the reusable tooling. Steady-state upstream `issue_comment` runs use trusted tooling from the workflow snapshot (`github.sha`), independently of the tested PR SHA. There is no separate override-removal PR.

## Permission Boundaries

| Component          | Authority                                                                                                               |
|--------------------|-------------------------------------------------------------------------------------------------------------------------|
| Manual maintenance | Cluster/network/registry/controller provisioning, IAM reconciliation and teardown                                       |
| Gate               | Actor authorization, strict command validation, source capture and request comment; no PR execution                     |
| Tooling preflight  | Read-only trusted capability validation; no PR execution, cloud credentials or GitHub writes                            |
| Build              | Credential-free preparation of PR controller/chart/test bundle; no OIDC or GitHub writes                                |
| Runtime            | Trusted coordinator and approved PR binaries/charts, dedicated-target WIF; no GitHub writes or setup/IAM administration |
| Reporter           | Fresh trusted tooling, bounded result data, check/comment writes; no cloud access or PR execution                       |

The workflow declares `permissions: {}` with explicit job permissions, SHA-pinned actions, bounded timeouts and 30-day artifacts. Shell commands receive validated inputs through environment variables rather than interpolated comment text. Trusted tooling does not share a PR-writable cache. A YAML job split is not itself a solution to privileged-code risks; review final CodeQL findings without bypasses or automatic suppression.

Cloud execution requires both supported capability and explicit `E2E_RUNTIME_ENABLED=true` with complete captured target/WIF configuration. The runtime job uses fixed protected environment `e2e-runtime` and displays both SHAs before approval. The owner must verify the environment exists with appropriate reviewers and deployment restrictions; YAML cannot prove operational protection.

WIF trust must bind the actual workflow-origin repository, trusted workflow/ref and protected environment. Checking out upstream tooling does not change OIDC claims. Fork rehearsal needs separately approved trust; never grant arbitrary fork workflows upstream credentials.

Audit transitive authority: runtime access, controller roles, Kubernetes RBAC, node identities and reachable workloads. Tests and charts are arbitrary approved code with meaningful target authority. Registry access must be scoped to its repository. Current manual setup grants controller `actAs` on the Compute default service account; a dedicated minimal node identity or equivalent effective-role proof is an activation prerequisite. Controller IAM remains defined only in `deploy/iam/karpenter-controller-role.yaml`.

## Separate GCP Terraform Prerequisite

Deliver an independent GCP-only root/state, planned at `deploy/terraform-e2e-ci/`, after or alongside #510 and before WIF-enabled rehearsal. It owns:

- GitHub WIF pool/provider with reviewed trust conditions.
- A distinct CI runtime service account and its federation grant.
- Audited additive runtime IAM members, referencing existing targets and registry.
- Outputs for manually configuring GitHub variables.

Do not reuse the existing cluster-provisioning Terraform root or duplicate resources owned by manual `e2e-setup`. Setup remains authoritative for its cluster, network, registry and controller resources. Avoid IAM policy/binding replacement and shared resource ownership. Manual teardown deletes underlying target resources; coordinate removal/reconciliation of dependent CI grants rather than silently importing them.

GitHub environments, variables and repository settings remain manually managed. Only configuration, dependency locks and safe examples enter Git. Actual state, plans, private inputs and credentials stay out of Git and CI artifacts; PR identities cannot access the state/backend or maintenance credentials. Apply/import/destroy, state migration and cloud configuration need separate owner approval.

Moving cluster creation to Terraform ([#556](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/556)) is explicitly deferred until the entire e2e series works. This prerequisite does not migrate cluster/network/setup infrastructure.

## Reusable Local Tooling — PR 2a

Keep existing suite packages and Ginkgo v2. Introduce a small root-module Go coordinator, not a framework or nested tools module. Shared local interfaces will be:

| Command           | Purpose                                                                                                  |
|-------------------|----------------------------------------------------------------------------------------------------------|
| `make e2e-plan`   | Read-only target/quota/inventory preview and deterministic plan                                          |
| `make e2e-run`    | Approved preflight, ownership-safe cleanup, one deployment, planned waves, diagnostics and final cleanup |
| `make e2e-report` | Offline report regeneration; never implicitly post to GitHub                                             |

These are follow-up interfaces, not implemented by #510. Keep focused `SUITE`/`FOCUS`, deploy/release and local key/ADC workflows. CI WIF exchange and GitHub publication belong outside the reusable runner. Never feed WIF `external_account` JSON to service-account-key activation. Local shared-cluster operations use the primary checkout's kubeconfig, not a worktree-local default.

### Planning and Execution

Standard discovers all non-GPU suites. Reviewed profiles must account for multi-node/replacement peaks, machine-family/regional/global CPU, addresses, storage and cleanup reserve. Unknown profiles, failed quota reads and insufficient capacity fail closed. The same snapshot/profiles/caps produce the same waves; never round zero capacity up to one.

Run independent suite packages concurrently and use Ginkgo spec workers within admitted suites. Record any quota/safety reason for one-suite/one-worker execution. Recheck capacity/clean state between waves. Reuse compatible precompiled Ginkgo suites and native JSON/JUnit reports.

Observe every child exit; stop new launches and terminate/reap children on infrastructure failure, cancellation or lease loss. Assertion failures cannot become green through automatic retries. No-tests, missing/malformed reports, interrupted runs and failed artifact creation are not success. Preserve time for diagnostics and cleanup within the overall timeout.

### Persistent-Target Lifecycle

GitHub target concurrency holds through runtime diagnostics/artifacts; it prevents CI overlap but provides neither durable FIFO nor local/maintenance exclusion. Before real execution, add a cooperative Kubernetes Lease and target-keyed local lock. Losing the lease stops work. After hard termination, the next run must detect leftover state.

Verify exact target, health and baseline before mutation. Drain owned workloads and NodeClaims while the previous controller still handles finalizers, then change controller state. Prove cloud deletion, not only Kubernetes object disappearance. Use known ownership labels/resource identities, never prefix-only deletion, blanket `--all`, automatic finalizer removal or infrastructure teardown. Unknown legacy resources require manual maintenance.

Deploy one immutable controller image with matching chart/CRD/test bundle and verify rollout/digest alignment before tests. Missing infrastructure, IAM or unhealthy state produces a maintenance-needed failure; runtime cannot repair it using setup credentials.

### Reporting

Preserve structured versioned run JSON, Ginkgo JSON/JUnit, suite logs, controller logs/events and log-scan findings. Public reports use `### E2E Test Results`, one row per full spec name, status/duration/notes, tested and tooling SHAs, zone, total duration, controller resource usage or explicit unavailability, overall result and artifact links. Escape untrusted text and omit credentials, project IDs and internal identifiers.

The bootstrap bridge limits tool stdout/stderr to 1 MiB and treats overflow as process failure. Follow-up tooling writes full logs/reports to files; bounded partial bridge diagnostics do not replace suite evidence. Novel warnings/errors require review rather than automatic benign classification.

## Pipeline Integration — PR 2b

Depend on PR 2a and the separately reviewed/applied CI prerequisite. Keep scheduling, lifecycle and report construction in shared tooling; YAML handles job boundaries, cloud authentication and publication.

Exercise candidate tooling through the merged harness using the mutable upstream branch. Distinguish tooling tests from workflow-YAML tests: candidate YAML still needs fork-default-branch rehearsal or post-merge verification. Prove standard execution, constrained waves, WIF longevity, failure handling, metrics/logs, source alignment and cleanup before activation.

In this PR remove the branch override and adopt the upstream workflow snapshot. After the user merges, verify `/e2e` on a subsequent upstream PR. Until then, report candidate validation and pending steady-state activation separately.

## Delivery and Acceptance

1. **#510 harness:** exact authorized commands, independent frozen identities, fail-closed capabilities/artifacts, permission boundaries, neutral bootstrap and truthful tested-SHA checks/comments. Rehearse on the fork default branch with upstream tooling, including branch A/B movement, invalid contracts/artifacts, controlled failure and publication errors. Cloud-free proof does not require a Terraform apply.
2. **Separate CI prerequisite:** review GCP-only Terraform definitions and independently approve any apply and manual GitHub configuration before WIF access.
3. **PR 2a local tooling:** deterministic fake-boundary tests, shared commands and an explicitly approved full local non-GPU proof, without a GitHub API dependency.
4. **PR 2b integration:** real CI evidence, runtime/transitive-identity audit, cooperative locking/cleanup and branch override removal together; verify steady state after merge.

Checks attach to the captured tested SHA in the triggering repository, never main's workflow SHA or an artifact-selected target. Sticky comments preserve user invocation comments and show both SHAs plus run/artifact links. Job, artifact, report and test failure overrides successful report generation. Publication errors leave a failure summary/comment where possible and fail the job; multi-API updates are not atomic. Hard cancellation may prevent final publication.

CLI and helper tests are included in `make ci` because existing `ut-test` covers only `pkg/`. Use fake cloud/process/API boundaries, Go race tests and workflow validation before any approved operational proof. CodeQL/review findings and environment/WIF protection remain independent gates.

All PR merges are user-owned, including fork rehearsal PRs. Do not enable auto-merge; stop for the user to review and merge. Branch creation, repository configuration, cloud operations, readiness and activation require explicit owner approval.

## Coverage and Later Work

Use [#250](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/250) for portable scheduling, validation, consolidation, drift/hash and expiration coverage, followed by GCP-specific Spot, image, maintenance, GPU, storage, networking and arm64 cases. GPU/full require separate capacity and safety validation; they are not implicitly enabled with standard.

An optional future KWOK lane can cover provider-agnostic behavior cheaply. It cannot validate Compute/GKE APIs, IAM, images, networking, quota or real node registration and is not a prerequisite for this series. Multiple targets and deeper queues require separate proposals when needed.

## Existing Patterns Reused

- Karpenter/Ginkgo: existing suites, spec scheduling, precompiled binaries and structured reports.
- AWS provider: short-lived cloud identity and artifact/check discipline, without copying ephemeral teardown.
- Azure provider: SHA-pinned actions and environment-variable indirection.
- IBM provider: existing-cluster testing, immutable image identity and ownership-aware diagnostics.

No evaluated package splitter supplies quota-aware admission plus this persistent-target lifecycle. Reuse Ginkgo and add only the missing coordination.

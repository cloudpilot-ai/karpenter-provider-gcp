# Proposal: Reusable E2E Tooling and On-Demand CI

- **Status**: Draft
- **Authors**: @dm3ch
- **Created**: 2026-06-21
- **Related Issues**: [#296](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/296), [#556](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/556)

## Summary

Build reliable local e2e orchestration first, provision a shared GCP environment with Terraform, then reuse the tooling from a manually triggered GitHub Actions pipeline. Add PR-comment commands last, as a thin wrapper around that pipeline.

Each stage ships a useful, independently testable result. Normal runs reuse a persistent GKE cluster; infrastructure creation and teardown remain explicit maintenance operations.

## Motivation

Maintainer-local e2e runs require manual quota planning, waved execution, log collection and result formatting. Building ChatOps before those operations are reusable adds integration complexity without delivering real test results. A shared runner makes local validation useful immediately and keeps local and CI behavior consistent.

## Proposal

### Shared runner and presets

Add a small Go coordinator around the existing Ginkgo suites, with `plan`, `run` and `report` operations. It schedules quota-safe suite waves and Ginkgo spec workers, verifies controller/chart/test revision alignment, and collects results and diagnostics. It does not require GitHub access or provision infrastructure.

| Preset               | Selection                                    |
|----------------------|----------------------------------------------|
| `standard` (default) | All non-GPU suites                           |
| `gpu`                | GPU suites only                              |
| `full`               | Standard and GPU suites, without duplication |

Use one preset definition across local, manual CI and ChatOps entry points. GPU execution is explicit; insufficient quota or capacity is reported, never silently converted to a smaller preset.

Save structured run JSON, native Ginkgo JSON/JUnit, suite logs and Karpenter controller logs. Render a per-spec Markdown results table from saved data using Go templates, including durations, failures, controller CPU/memory samples and log findings. Reports can be regenerated offline. Resource summaries describe observed run/wave usage, not precise per-spec attribution; missing samples are explicit. Optional pprof collection is follow-up work.

### Infrastructure and execution boundaries

Terraform manages persistent e2e resources in the GCP project provided by PlanetScale: cluster, networking, registry, identities and WIF. Maintenance credentials and Terraform state are separate from runtime access. Resource ownership must not overlap with existing setup scripts. Agree trusted workflow identity and WIF claim restrictions before enabling grants; the pipeline stage implements and verifies that contract.

A maintainer selects a PR and preset in the manual pipeline. The pipeline captures the PR head SHA once and builds the matching controller, chart and tests without cloud credentials or GitHub write access. Trusted orchestration uses a captured upstream workflow/tooling revision, separate from the code under test. Runtime uses short-lived WIF credentials scoped to the dedicated target; a separate trusted reporter publishes results without executing PR code or accessing GCP.

Checks identify the tested SHA. PR reports include the preset, revision, results and artifact links; artifacts are retained for 30 days. Public output excludes credentials and internal infrastructure identifiers. Publication failure is visible and cannot turn a failed run into success.

ChatOps accepts `/e2e`, `/e2e standard`, `/e2e gpu` and `/e2e full` only from users with `write`, `maintain` or `admin` permission. It invokes the same pipeline with the captured latest PR SHA, not a second orchestration implementation.

### Required safety properties

- Lock the existing target across local, CI and supported legacy entry points; retain ownership through diagnostics and cleanup. Initial provisioning and cluster outages need an explicit exclusive maintenance procedure, not a Kubernetes lock that cannot be acquired.
- Check quota and cluster health before mutation. Use bounded waves, timeouts and cancellation; preserve partial results on failure.
- Clean only proven test-owned resources, allowing the existing controller to drain finalizers before replacement. Never repair IAM, force finalizers or tear down infrastructure during normal runs.
- Separate maintenance, build, runtime and reporting authority. Audit reachable controller and node identities as well as the CI identity; this is approved-code execution, not a sandbox.
- Authorize the exact captured revision before cloud execution. Later PR pushes require another invocation; third-party actions are SHA-pinned and untrusted inputs are validated.
- Distinguish test failure, infrastructure failure, cancellation and incomplete execution. Missing reports or zero executed tests cannot count as success. Automatic test retries are deferred.

## Staged Delivery and Acceptance Criteria

Each stage is reviewed separately; later stages do not block shipping earlier useful work. Maintainers may explicitly approve starting ChatOps once manual standard runs work, while stage 3 remains partial and unvalidated GPU/full modes stay disabled. Final completion still requires live evidence for all three presets unless maintainers explicitly revise the scope.

| Stage                    | Deliverable                                                                | Completion evidence                                                                                                                                                                                              |
|--------------------------|----------------------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| 0. Proposal              | This proposal, separate from implementation changes                        | Maintainer review and merge agree the scope, stages and ownership.                                                                                                                                               |
| 1. Local tooling         | Go wave runner, presets, saved results, templated report, metrics and logs | Real local standard run on the existing environment; deterministic tests cover presets, quota limits, failure/cancellation and cleanup. GPU/full live validation is explicitly approved and recorded separately. |
| 2. Terraform environment | Reproducible persistent environment and WIF resources; resolves #556       | Approved provisioning, repeat plan without unexpected changes, runtime/maintenance permission checks, and the local runner works against the new target.                                                         |
| 3. Manual pipeline       | Maintainer-triggered PR/preset execution with WIF and PR reporting         | Real aligned run, controlled failure, retained diagnostics, tested-SHA check, shared-target exclusion and working cleanup; all three presets have live evidence before this stage is complete.                   |
| 4. ChatOps               | Permission-checked comments invoking the existing pipeline                 | Authorization, malformed input, PR-push race and preset tests; a real post-merge comment-triggered run uses the captured SHA.                                                                                    |
| 5. Completion            | Mark the proposal implemented and record follow-up issues                  | Maintainers can operate and recover the system; standard, gpu and full have live evidence, all stage outcomes are verified, and remaining limitations are recorded.                                              |

## Migration

Keep existing local and release workflows working while the runner is introduced. Validate it against the current environment before requiring the new Terraform target. Confirm sponsored-project quotas, machine availability, budget and ownership before provisioning; budget alerts are not spending caps.

Provision the new environment separately, or explicitly plan imports if resources already exist. Switch targets only after validation; retain the old environment until its owner approves retirement. Setup scripts must delegate to Terraform or stop managing Terraform-owned resources. GitHub environment/WIF configuration and infrastructure apply/destroy require separate approval. Normal test execution never calls setup.

Existing ChatOps work can be reused selectively when stage 4 is reached. A temporary tooling-branch override and placeholder execution harness are not prerequisites for this roadmap.

## Alternatives Considered

- **ChatOps/harness first:** rejected because it requires temporary contracts and trust configuration before useful execution exists.
- **Separate local and CI runners:** rejected because scheduling, cleanup and reports would diverge.
- **Replace Ginkgo or build a general workflow engine:** unnecessary; reuse native execution and reports, adding only quota-aware coordination and lifecycle handling.

## Future Direction

Automatic retries with visible attempt history, resumable waves, optional pprof, historical scheduling optimization, multiple targets and a durable request queue are follow-ups. Mutual exclusion does not promise FIFO delivery. Broader coverage and a possible KWOK lane remain in [#250](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/250), outside this delivery plan.

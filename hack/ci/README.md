# E2E CI harness

This directory contains the #510 ChatOps harness, **not a working e2e runner**. The bootstrap advertises standard mode as `not_implemented`. A successful **E2E harness validation** check with a neutral **E2E standard** check does not mean cloud tests passed.

## Commands and sources

Users with repository `write`, `maintain`, or `admin` permission can post exactly `/e2e`, `/e2e standard`, `/e2e gpu`, or `/e2e full` on a PR. Extra arguments, whitespace and multiline commands are rejected before execution. GPU/full report not implemented without resolving tooling, taking the runtime lock or authenticating.

For standard mode, an administrator sets `E2E_TOOLING_REF` to an existing branch such as `e2e-ci-bootstrap` in **cloudpilot-ai/karpenter-provider-gcp**. The gate resolves exactly `refs/heads/<branch>` once, independently captures the PR repository/full SHA, and freezes both with contract version, triggering repository/PR, run/attempt and target configuration. There is no fallback to a PR/fork tooling ref. A running invocation retains version A after a branch update; a later invocation captures version B.

Write access to that upstream branch grants trusted tooling execution, including reporting. Review its contents. The workflow bridge comes from the triggering workflow snapshot; the tooling CLI is built afresh from the captured upstream SHA in each consuming job. A PR-build artifact is never trusted orchestration. Both SHAs appear in comments and the runtime approval job name.

## Job boundaries

| Job               | Authority                                                                                              |
|-------------------|--------------------------------------------------------------------------------------------------------|
| Gate              | GitHub command authorization, source capture and request comment; no PR execution                      |
| Tooling preflight | Read-only capability validation; no PR code or cloud access                                            |
| Build             | PR code and captured tooling, read-only repository access; no OIDC or GitHub writes                    |
| Runtime           | Captured tooling plus approved code-under-test bundle; target WIF only, no GitHub writes               |
| Reporter          | Fresh captured tooling, validated result data, GitHub checks/comments; no PR execution or cloud access |

Unsupported, disabled, missing and incompatible contracts cannot reach build, runtime concurrency or cloud authentication. Missing/malformed contracts fail; unsupported/disabled execution is neutral. The runtime uses fixed environment `e2e-runtime`, target-keyed concurrency, and a 60-minute timeout. Native Actions concurrency is mutual exclusion, not a durable FIFO: an older pending request can be replaced. It does not exclude local or maintenance operations.

## Administrative prerequisites

Leave `E2E_RUNTIME_ENABLED` unset or false during cloud-free harness rehearsal. Even setting it true cannot make the bootstrap runner execute tests.

Before any WIF/cloud activation, the owner must:

- Provision/reconcile the target manually with `make e2e-setup`; never call setup or teardown from normal runtime. Current setup accepts a service-account key, not a WIF `external_account` file.
- Review/apply the **separate GCP-only Terraform prerequisite PR**, planned under `deploy/terraform-e2e-ci/`. It owns WIF pool/provider, a distinct CI runtime service account and audited additive grants. It does not own the existing cluster/network/registry/controller resources. Its state/backend and maintenance credentials must be unreachable by PR identities. No Terraform stack is implemented in #510.
- Manually create and protect `e2e-runtime` with required reviewers and trusted workflow/ref deployment restrictions. Naming an environment in YAML does not prove that protection exists.
- Bind WIF to the actual workflow-origin repository, trusted workflow/ref and protected environment. A tooling checkout does not change OIDC claims. Fork rehearsal needs separately approved trust, not a grant for arbitrary forks.
- Audit runtime, controller, node and workload identities transitively, including Kubernetes RBAC and registry scope. Existing setup grants controller `actAs` on the Compute default service account: verify minimal effective roles or replace it in follow-up tooling before activation. No identity may reach maintenance, project-IAM or cross-project authority.
- Validate shared/local locking, ownership-safe cleanup, quota planning, matching controller/chart/test sources and WIF longevity in the follow-up runner. Approved arbitrary code is not sandboxed by this harness.

Set these repository variables manually; the gate captures them once:

| Variable                                                     | Meaning                                                        |
|--------------------------------------------------------------|----------------------------------------------------------------|
| `E2E_TOOLING_REF`                                            | Required upstream tooling branch for standard mode             |
| `E2E_RUNTIME_ENABLED`                                        | Exact `true` only after runtime approval                       |
| `E2E_PROJECT_ID`, `E2E_REGION`, `E2E_LOCATION`, `E2E_PREFIX` | Explicit dedicated target, required when enabled               |
| `GCP_E2E_WORKLOAD_IDENTITY_PROVIDER`                         | Approved provider resource name, required when enabled         |
| `GCP_E2E_SERVICE_ACCOUNT`                                    | Dedicated runtime service-account email, required when enabled |

GitHub variables, environments and settings are not Terraform-managed by the prerequisite. Manual teardown deletes the registry/cluster underlying CI grants; coordinate removal/reconciliation of dependent CI state rather than introducing dual resource ownership. Moving cluster creation into Terraform ([#556](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/556)) is deferred until the entire series works.

## Version 1 tooling contract

The CLI lives in the root Go module:

```sh
go run ./hack/ci/e2e-runner capabilities
make ci-harness-test
```

`capabilities` writes JSON to stdout:

```json
{"version":1,"commands":["capabilities","prepare","run","report"],"modes":{"standard":"not_implemented"}}
```

A future reviewed implementation advertises `supported` only when its CI adapters are usable. `prepare`, `run` and `report` accept `--invocation <json>`, `--source <directory>`, `--bundle <directory>` and `--output <json>`. Source is relevant to prepare; bundle holds code under test for run and result data only for report. Commands must return nonzero on process/infrastructure failure. The bootstrap only emits neutral, zero-execution JSON; it does not prepare a runnable bundle.

- `prepare` produces `bundle/manifest.json`: `{ "version": 1, "invocation": <exact captured object>, "files": [{ "path": "controller", "sha256": "<64 lowercase hex characters>" }] }`. Include matching PR controller/chart/test files; never a trusted runner.
- Bundle paths are relative regular files, unique, allowlisted characters, no traversal or symlinks; unlisted files fail. JSON is limited to 1 MiB, inventory to 10,000 files and total declared file data to 4 GiB. The bridge does not extract nested archives.
- Artifact IDs come from the same run's upload step, not a latest-artifact query. Manifest digests are passed separately through job outputs and verified alongside the entire invocation and file digests before WIF. These are integrity checks, not a sandbox or an endorsement of PR binaries.
- `run` writes `{ "version": 1, "invocation": <exact captured object>, "status": "passed|failed|not_implemented", "executed": <nonnegative integer> }`. Passed requires a positive count; not implemented requires zero. Structured suite evidence and richer reports belong to the follow-up runner.
- Runtime packages only `result.json` in a separate identity-bound manifest. Reporter consumes bounded data, never uploaded scripts. `report` emits the same result contract and cannot change status or executed count. Gate/event identities—not artifact fields—select the reporting repository, PR and tested SHA.

Tool subprocess stdout/stderr have a 1 MiB process-output limit; overflow, timeout or nonzero exit fails the command. Subprocess deadlines are 2 minutes for capabilities/report, 20 for prepare and 45 for run, below their enclosing job budgets to reserve time for diagnostics. Deadline termination is forced; future runtime tooling must budget its own graceful cleanup before that limit. Bounded partial diagnostics are saved, including failures, and retained for 30 days alongside bundles/results. Hard job cancellation or runner loss can still prevent collection/upload. Future tooling must write full suite logs/reports to files rather than streaming them through this bootstrap bridge; never include credentials or raw environment dumps. Preflight/reporter skip diagnostic upload when no tool was invoked. Upload failures remain failures.

## Results and recovery

Checks attach to the captured tested SHA, not the default-branch workflow SHA. Sticky request/results comments stay on the triggering repository/PR, including fork rehearsal. They preserve user invocation comments and include both SHAs plus workflow/artifact links.

Job, artifact and test failures override successful report generation. Failed check publication leaves a failure comment/summary and a failed reporter; GitHub API writes are not atomic. If source capture fails, the reporter posts configuration failure without inventing a tested SHA. Hard workflow cancellation can prevent reporting; inspect the run instead of assuming a stale comment describes it.

For malformed contracts, missing tooling branches or invalid artifacts, inspect the failed job and retained diagnostics, correct the trusted branch/configuration and submit a new command. Do not enable broader credentials or run setup as a repair from CI. Real target cleanup/maintenance remains an explicit maintainer operation.

## Delivery sequence

1. **#510:** review the harness, then rehearse it on the fork default branch with upstream tooling. Prove authorization, A/B branch capture, neutral execution, invalid contracts/artifacts, controlled failure and reporting without claiming real e2e success.
2. **Separate prerequisite PR:** GCP-only Terraform definitions and separately approved apply, before WIF-enabled rehearsal. Keep setup and GitHub configuration manual.
3. **PR 2a:** reusable local plan/run/report tooling, Ginkgo coordination and ownership-safe lifecycle; prove it locally without requiring GitHub API access.
4. **PR 2b:** integrate that tooling and validate real standard CI. Remove `E2E_TOOLING_REF` and its branch-override path in the same PR, switching trusted tooling to the upstream `issue_comment` workflow snapshot (`github.sha`). Verify steady-state operation after merge.

All PR merges are performed by the user after review, including fork rehearsal PRs. Do not enable auto-merge. Branch creation, repository configuration, cloud operations and activation require separate owner approval.

# Proposal: On-Demand E2E CI for Pull Requests

- **Status**: Draft
- **Authors**: @dm3ch
- **Created**: 2026-06-21
- **Related Issues**: [#296](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/296), [#556](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/556)

---

## Summary

PlanetScale now provides the GCP project for this repository's e2e infrastructure. This proposal defines an e2e roadmap for on-demand real GKE CI ([#296](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/296)): first ship useful local tooling, then provision the shared environment with Terraform, add a manual WIF pipeline, and finally add PR-comment commands as a thin wrapper.

The local runner tests the current checkout against a controller built from the same commit on a persistent GKE e2e cluster. Later CI captures an immutable PR head SHA, deploys that revision, and publishes a PR comment plus a GitHub check. ChatOps accepts `/e2e`, `/e2e gpu`, or `/e2e full` only after that pipeline exists.

The real GKE cluster is not torn down after each run. Reusing the cluster should keep latency low and cost acceptable. Existing suites clean their own test resources; environment-wide cleanup remains an explicit maintenance operation. A separate KWOK fast lane may be added for cheap provider-agnostic coverage on ready PRs, but it does not replace real GKE validation.

---

## Motivation

E2E validation is currently a maintainer-local process. That makes results harder to audit, harder to reproduce, and unavailable to contributors without maintainer credentials.

Issue [#296](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/296) asks for a maintainer-approved e2e flow in GitHub Actions: secrets remain in the base repo, untrusted contributor code runs only after maintainer approval, and the tested revision is pinned by SHA.

### Goals

- Maintainer-triggered real GKE e2e runs from PR comments.
- Secure base-repo access to GCP credentials.
- Immutable SHA checkout and deploy/test alignment verification.
- Local `standard`, `gpu`, and `all` selections; `/e2e full` maps to `all` in later CI.
- Persistent GKE infrastructure; no normal per-run teardown.
- A reusable local runner before CI integration.
- Terraform-managed shared infrastructure that resolves [#556](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/556).
- PR-visible results and GitHub checks.

### Non-Goals

- Auto-running e2e on every PR push.
- Replacing local maintainer debugging workflows.
- Expanding coverage or adding a KWOK lane; those remain follow-up work under [#250](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/250).
- Treating KWOK as a substitute for real GKE e2e.

---

## Proposal

### User Interface

| Command         | Runner selection | Runs                       |
|-----------------|------------------|----------------------------|
| `/e2e`          | `standard`       | All non-GPU features       |
| `/e2e standard` | `standard`       | All non-GPU features       |
| `/e2e gpu`      | `gpu`            | GPU feature only           |
| `/e2e full`     | `all`            | Standard features plus GPU |

Locally, `make e2e-tests E2E_SELECTION=...` accepts `standard` (the default), `gpu`, `all`, `provisioning`, or comma-separated feature directories. `E2E_PRESET` remains a legacy alias. The later ChatOps gate accepts pull request comments whose body is `/e2e` or starts with `/e2e `; malformed commands must not touch GCP.

### Initial Infrastructure Model

Terraform provisions and owns the persistent GKE environment in the PlanetScale-provided project, including the resources needed for WIF. The stage that implements [#556](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/556) chooses the target location only after maintainer approval of quotas, capacity, budget and ownership. GPU mode is enabled only after its required capacity is validated.

Keep the mode-to-cluster abstraction even while all modes target one cluster. If future quota or parallelism requires it, GPU or standard runs can move to separate clusters by changing configuration, not runner logic.

CI must pass `E2E_REGION`, `E2E_LOCATION`, `E2E_PROJECT_ID`, and `E2E_PREFIX` explicitly to every reused setup, deploy, image publishing, and test command. CI scripts must fail fast if any required target input is missing rather than relying on local-development defaults.

### Repository Layout and Ownership

Start with the local Go runner and keep most later CI logic outside YAML. When the manual pipeline is added, it can use one workflow; the later ChatOps entry point dispatches that pipeline rather than adding a second execution path.

```text
hack/e2e-runner/                 # local Go runner for selection, execution and reporting
<terraform environment>/          # persistent environment and WIF resources
.github/workflows/<manual>.yaml  # later manual pipeline
.github/workflows/<chatops>.yaml # later permission-checked dispatcher
```

Terraform, rather than setup scripts, owns persistent infrastructure. The exact workflow split is deferred until the manual-pipeline stage.

The local runner owns selection, one global Ginkgo run, a Kubernetes Lease, controller commit checks, and a Markdown report with controller logs. The later manual pipeline handles trusted event wiring, permissions, OIDC/WIF setup, immutable PR-SHA checkout, concurrency and artifact upload; ChatOps only dispatches that pipeline.

Workflow, runner and shell orchestration run from trusted default-branch code. Pull request workflow files and scripts must not run before authorization. After authorization, the later pipeline may build and deploy the pull request's controller and chart and run its tests at the resolved immutable SHA; that code runs only with the dedicated e2e project and least-privilege service accounts described here.

Bash should remain glue: validate required environment variables, set strict shell options, call `gcloud`, `kubectl`, `ko`, `helm`, and `go test`, and preserve logs/artifacts.

The Go runner selects features by Ginkgo labels, invokes the existing suites, and renders native Ginkgo JSON into Markdown. Local JSON is temporary; durable CI artifacts and publication belong to the later pipeline. A later pinned IssueOps action may own coarse command detection, GitHub permission checks and immutable SHA discovery.

The existing `e2e/` package remains focused on test definitions and shared test utilities. CI-specific orchestration should not be melted into `e2e/`; the gate and future runner invoke the tests from outside so local e2e development is not coupled to GitHub comments or artifact/reporting concerns.

All first-version CI code and infrastructure setup scripts live in this repository.

### Authentication

Use GitHub Actions OIDC to GCP Workload Identity Federation:

```text
GitHub Actions OIDC token
→ GCP Workload Identity Federation provider
→ dedicated e2e CI service account
→ short-lived credentials for gcloud/kubectl/ko/helm
```

No long-lived JSON key should be needed.

The PR-triggered CI identity must be limited to the e2e project and e2e resources. A separate maintenance identity can own rare project-wide setup or teardown tasks.

### Security Model

Required controls:

- Accept triggers only on pull request comments; ignore or reject `/e2e` comments on normal issues before any GCP authentication or SHA resolution.
- Accept triggers only from users with `write`, `maintain`, or `admin` repository permission.
- Resolve the PR head SHA at trigger time; later execution phases must test that immutable SHA and never test a mutable branch ref.
- Run workflow, runner, orchestration, and e2e test code from the trusted default branch.
- Do not run PR-controlled workflow code, scripts, or tests before authorization.
- Treat PR title, commit messages, labels, file names, and workflow inputs as untrusted.
- Pin third-party GitHub Actions by full commit SHA.
- Pass GitHub context/secrets through environment variables before shell use, then quote them.
- Keep secrets out of public comments and logs.
- Use job timeouts, bounded parallelism, bounded retries, and per-cluster concurrency.
- Never call `make e2e-teardown` from normal PR-triggered runs.

Network egress hardening can be evaluated only if it works on GitHub-hosted runners without paid SaaS or self-hosted infrastructure. Otherwise, document that it is out of scope.

### Locking and Persistent Cluster Hygiene

The local runner uses a Kubernetes Lease in the controller namespace to reject overlapping runner-managed tests on the same cluster. It holds the Lease through test execution, controller log collection and report writing; interruption cancels the run and releases it, while a forced kill leaves it to expire. It verifies the kubeconfig context and deployed controller image/binary against the test checkout before executing specs. The existing suites own per-spec cleanup. `e2e-setup`, `e2e-deploy`, `e2e-clean-env` and standalone `e2e-test` do not share this Lease: operators must not run them concurrently with runner-managed tests. The runner never runs setup, teardown, deploy or environment-wide cleanup implicitly.

For CI, acquire a per-target job-level `concurrency` group before cloud mutation and keep it through diagnostics and any explicit cleanup. Rejected commands must not queue. CI must coordinate deployment/cleanup with the local Lease or an enforced maintenance exclusion; job concurrency alone does not protect against local operators. Verify the target and available quota before choosing `GINKGO_PROCS`; if cleanup or infrastructure checks fail, stop and require explicit maintenance rather than blindly deleting resources.

### Test Parallelism

The local runner runs one unified Ginkgo suite with feature labels and a single `GINKGO_PROCS` limit (default 4) across all selected specs. `standard` excludes GPU; `gpu` selects GPU; `all` includes both; `provisioning` and explicit feature lists support local debugging. No separate suite waves, automatic quota planner, or GPU environment toggle is used. Before running, operators check address, SSD, regional/all-regions CPU and GPU quota/capacity as applicable, then lower the global limit if needed. CI must apply the same bounded selection and capacity checks before enabling GPU or full runs; automated quota-aware waves remain a possible follow-up.

### Retries

Retries are intentionally limited:

- Transient setup/deploy operations may retry with backoff.
- Deploy alignment mismatch may redeploy once, then fail before tests.
- Quota exhaustion and GPU capacity failures do not loop; report them as infrastructure failures.
- Test assertion failures do not become green because of automatic reruns. A future flake-confirmation rerun may be added, but both attempts must be reported and the first failure must remain visible.
- Maintainers can always post the command again for a manual rerun.

### Reporting

The local runner writes an ignored Markdown report with feature/spec results, durations, controller resource samples from the suites when available, and a companion controller log. The Ginkgo JSON used to render the report is temporary; the runner does not publish to GitHub. The later CI pipeline produces:

1. GitHub check on the tested SHA.
2. PR comment replacing the previous bot-authored result for that mode or response type. User invocation comments are preserved.
3. Artifacts retained for 30 days:
   - JUnit XML per suite/attempt,
   - JSON run summary,
   - plain-text suite logs,
   - controller log scan and diagnostics.

PR comments include mode, commit, duration, suite/spec result table, controller resource samples when available, Karpenter panic/error scan result, overall status, and artifact links. They must not include GCP project IDs, credential paths, tokens, or raw environment dumps.

---

## Existing Karpenter CI Patterns

The design intentionally borrows from nearby projects:

| Project                                        | Useful pattern                                                                     | GCP decision                                                               |
|------------------------------------------------|------------------------------------------------------------------------------------|----------------------------------------------------------------------------|
| `kubernetes-sigs/karpenter`                    | Reusable workflows, kind/KWOK, pinned actions, 30-day artifacts                    | Reuse workflow/artifact discipline; cloud e2e still needs real GCP.        |
| `aws/karpenter-provider-aws`                   | OIDC to cloud identity, suite matrix, jitter, commit statuses, failure log dump    | Use GCP WIF equivalent; add jitter/backoff; keep checks/statuses.          |
| `Azure/karpenter-provider-azure`               | SHA-pinned actions, env-var indirection for shell safety                           | Adopt security conventions where compatible.                               |
| `kubernetes-sigs/karpenter-provider-ibm-cloud` | Existing-cluster PR e2e, SHA-derived image tags, label/tag cleanup, rich artifacts | Reuse persistent-cluster and cleanup ideas; keep GCP parallelism stronger. |

For later CI integration, adopt:

- reusable workflow + portable runner scripts,
- GitHub OIDC/WIF cloud auth,
- SHA-pinned actions,
- env-var indirection for shell safety,
- per-cluster locking,
- failure diagnostics under `if: always()`,
- JUnit/JSON/log artifacts with 30-day retention,
- ownership-label-gated cleanup, with name prefixes used only as diagnostic hints.

Defer until needed:

- CI command expansion beyond `standard`/`gpu`/`full` (local feature selection already exists),
- strict egress allowlist if it requires paid SaaS or self-hosted infrastructure,
- multi-project leasing.

We should not copy ephemeral cluster teardown from AWS/Azure because this proposal intentionally keeps GKE infrastructure alive between runs.

### KWOK

KWOK is useful for cheap, fast validation of provider-agnostic Karpenter behavior: scheduling logic, NodePool/NodeClaim controller interactions, disruption flows, and report/runner plumbing. It is not a replacement for this proposal's GKE e2e CI because it cannot validate GCP Compute API calls, GKE bootstrap metadata, IAM, images, networking, disks, GPUs, quota, or actual node registration.

KWOK may be considered later under [#250](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/250), not as a prerequisite for the first real GKE CI PR. A useful future split is:

- **GKE lane**: maintainer-triggered, real GCP/GKE behavior, required for merge confidence.
- **KWOK lane**: a possible future cheap fast e2e for ready PRs or every trusted push.

The KWOK lane can eventually support issue #250 by covering portable scheduling/disruption scenarios, while GCP-specific scenarios still run only in the GKE lane.

---

## Risks and Mitigations

| Risk                                        | Mitigation                                                                                                                                             |
|---------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------|
| Malicious PR burns quota or mines resources | trigger restricted to users with write-or-higher repository permission, dedicated project, quotas/budget alerts, timeouts, bounded parallelism/retries |
| Secret exfiltration                         | WIF short-lived credentials, least-privilege service account, no unrelated secrets in env                                                              |
| Wrong commit tested                         | resolve SHA at trigger time; verify deployed image and embedded commit                                                                                 |
| Persistent cluster leaks resources          | per-spec cleanup and explicit local cleanup/audit; coordinate guarded pre/post cleanup in later CI and document manual maintenance                     |
| GPU capacity unavailable                    | separate `gpu` selection; check capacity before local runs and validate it before enabling CI GPU                                                      |

---

## Coverage Follow-up

Coverage expansion and a possible KWOK lane remain tracked separately in [#250](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/250). They are not prerequisites for this delivery sequence and do not replace real GKE validation.

---

## Acceptance Criteria

- [X] Phase 1 local runner implemented on the feature branch: `standard`, `gpu`, `all` and feature selections, globally bounded Ginkgo execution, Lease, commit checks and local Markdown report; merge and live validation remain pending.
- [ ] Terraform reproducibly owns the persistent environment and WIF resources, resolving #556 without conflicting with setup scripts.
- [ ] A maintainer-triggered pipeline checks out and tests an immutable PR head SHA and authenticates to GCP through OIDC/WIF.
- [ ] ChatOps accepts `/e2e`, `/e2e gpu`, and `/e2e full` only from users with `write`, `maintain`, or `admin` permission, and dispatches the existing pipeline.
- [ ] The workflow verifies deploy/test alignment before running tests.
- [X] Standard mode uses one global Ginkgo spec-worker limit; operators check quota and adjust it before local runs. Automated quota admission is not part of Phase 1.
- [ ] Normal runs reuse infrastructure and do not perform teardown.
- [ ] Failures distinguish test failures from infrastructure/setup/quota/capacity failures.
- [ ] Reports include GitHub checks, Markdown PR comments, JUnit XML, JSON summaries, logs, and artifact links.
- [ ] Artifacts are retained for 30 days.
- [ ] Third-party actions are pinned by SHA and shell steps avoid direct unquoted GitHub-context interpolation.
- [ ] First-version CI and runner code lives in this repository.
- [ ] Documentation explains trigger syntax, permissions, modes, security model, troubleshooting, and maintenance.
- [ ] Standard, GPU and full runs have live evidence before the proposal is marked implemented; coverage follow-up stays under #250.

---

## Implementation Phases

### Phase 1 — Local Tooling (implemented on feature branch)

`make e2e-tests` uses `hack/e2e-runner`: feature selection, a global Ginkgo worker limit, Kubernetes Lease, checkout/controller alignment, Markdown results and controller logs. Existing Ginkgo suites retain their cleanup; setup/deploy/maintenance remain separate. The runner is useful locally without GitHub. Merge and a separately approved live standard run are still required before treating this phase as operationally validated.

### Phase 2 — Terraform Environment

Implement #556 with a Terraform-managed persistent environment and WIF resources. Validate the local runner against the new target.

### Phase 3 — Manual Pipeline

Add maintainer-triggered PR/preset execution with WIF, immutable-SHA alignment, artifacts and PR reporting.

### Phase 4 — ChatOps

Add permission-checked PR comments that invoke the existing manual pipeline. Do not create a second orchestration path.

### Phase 5 — Completion

Record live standard, GPU and full evidence, mark the proposal implemented, and file agreed follow-ups.

---

## Future Direction

- Multiple clusters for parallel runs.
- `/e2e retry` for maintainer-requested reruns.
- Periodic maintenance workflow for explicit cleanup or cluster rebuilds.

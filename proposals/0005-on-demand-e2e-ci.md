# Proposal: On-Demand E2E CI for Pull Requests

- **Status**: Draft
- **Authors**: @dm3ch
- **Created**: 2026-06-21
- **Related Issues**: [#296](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/296), [#250](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/250)

---

## Summary

PlanetScale now provides the GCP project for this repository's e2e infrastructure. This proposal defines an e2e roadmap that covers both on-demand real GKE CI ([#296](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/296)) and test coverage growth toward AWS-provider parity ([#250](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/250)).

A maintainer posts `/e2e-test`, `/e2e-test gpu`, or `/e2e-test full` on a PR. GitHub Actions verifies the commenter has write access, resolves the PR head SHA, checks out that immutable SHA, deploys that exact commit to a persistent GKE e2e cluster, runs the requested suites, and publishes a PR comment plus a GitHub check.

The real GKE cluster is not torn down after each run. Reusing the cluster should keep latency low and cost acceptable. Runs still clean Kubernetes and Karpenter-created resources before and after test execution. A separate KWOK fast lane may be added for cheap provider-agnostic coverage on ready PRs, but it does not replace real GKE validation.

---

## Motivation

E2E validation is currently a maintainer-local process. That makes results harder to audit, harder to reproduce, and unavailable to contributors without maintainer credentials.

Issue [#296](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/296) asks for a maintainer-approved e2e flow in GitHub Actions: secrets remain in the base repo, untrusted contributor code runs only after maintainer approval, and the tested revision is pinned by SHA.

### Goals

- Maintainer-triggered real GKE e2e runs from PR comments.
- Secure base-repo access to GCP credentials.
- Immutable SHA checkout and deploy/test alignment verification.
- Separate modes for standard, GPU, and full runs.
- Persistent GKE infrastructure; no normal per-run teardown.
- Optional KWOK fast lane for provider-agnostic e2e signal on ready PRs.
- A phased path to expand e2e coverage toward AWS-provider parity.
- PR-visible results and GitHub checks.

### Non-Goals

- Auto-running e2e on every PR push.
- Replacing local maintainer debugging workflows.
- Completing all AWS-provider parity tests in the first PR.
- Treating KWOK as a substitute for real GKE e2e.

---

## Proposal

### User Interface

| Command              | Mode       | Runs                                      |
|----------------------|------------|-------------------------------------------|
| `/e2e-test`          | `standard` | All non-GPU suites                        |
| `/e2e-test standard` | `standard` | All non-GPU suites                        |
| `/e2e-test gpu`      | `gpu`      | GPU suite only, with `E2E_GPU_TESTS=true` |
| `/e2e-test full`     | `full`     | Standard suites plus GPU                  |

Only exact single-line commands matching `^/e2e-test\s*(standard|gpu|full)?\s*$` start a run. Malformed commands get a short help response and do not touch GCP.

### Initial Infrastructure Model

Start with one persistent GKE cluster for all modes:

```text
E2E_PROJECT_ID=<sponsored project from secrets/vars>
E2E_PREFIX=karpenter-e2e
E2E_LOCATION=asia-southeast1-b
E2E_REGION=asia-southeast1
```

`asia-southeast1-b` is preferred because it can potentially satisfy standard, full, and GPU runs while avoiding known `asia-southeast1-a` ARM-machine issues in the e2e set. GPU mode is enabled only after maintainers validate both `nvidia-l4` capacity and required ARM machine availability in this zone.

Keep the mode-to-cluster abstraction even while all modes target one cluster. If future quota or parallelism requires it, GPU or standard runs can move to separate clusters by changing configuration, not runner logic.

CI must pass `E2E_REGION`, `E2E_LOCATION`, `E2E_PROJECT_ID`, and `E2E_PREFIX` explicitly to every reused setup, deploy, image publishing, and test command. CI scripts must fail fast if any required target input is missing rather than relying on local-development defaults.

### Repository Layout and Ownership

Use one GitHub Actions workflow for the first version and keep most logic outside YAML:

```text
.github/workflows/e2e.yaml       # issue_comment trigger, auth, WIF, concurrency, artifacts
hack/ci/e2e.sh                   # shell entrypoint and CI glue
hack/ci/e2e-clean.sh             # cleanup and health checks
hack/ci/e2e-infra/               # manual/one-time infrastructure setup scripts
hack/tools/e2e-runner/           # Go runner for parsing, mode selection, reporting
```

A split workflow (`e2e-comment.yaml` dispatching `e2e-run.yaml`) is deferred until there is a concrete reuse need such as scheduled runs, manual dispatch, or separate trusted-push lanes. The first implementation should avoid splitting only for abstraction.

YAML should handle event wiring, permissions, OIDC/WIF setup, checkout of the immutable SHA, concurrency, and artifact upload. It should not contain substantial command parsing, reporting, or test orchestration logic.

Bash should remain glue: validate required environment variables, set strict shell options, call `gcloud`, `kubectl`, `ko`, `helm`, and `go test`, and preserve logs/artifacts.

The Go runner owns structured CI behavior: `/e2e-test` command parsing, mode validation, test suite selection, cleanup planning, result classification, JSON summary generation, Markdown PR comment rendering, and artifact indexing.

The existing `e2e/` package remains focused on test definitions and shared test utilities. CI-specific orchestration should not be melted into `e2e/`; the runner invokes the tests from outside so local e2e development is not coupled to GitHub comments or artifact/reporting concerns.

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

- Accept triggers only on pull request comments; ignore or reject `/e2e-test` comments on normal issues before any GCP authentication or SHA resolution.
- Accept triggers only from users with `write`, `maintain`, or `admin` permission.
- Resolve and test the PR head SHA at trigger time; never test a mutable branch ref.
- Do not run PR-controlled workflow code before authorization.
- Treat PR title, commit messages, labels, file names, and workflow inputs as untrusted.
- Pin third-party GitHub Actions by full commit SHA.
- Pass GitHub context/secrets through environment variables before shell use, then quote them.
- Keep secrets out of public comments and logs.
- Use job timeouts, bounded parallelism, bounded retries, and per-cluster concurrency.
- Never call `make e2e-teardown` from normal PR-triggered runs.

Network egress hardening can be evaluated only if it works on GitHub-hosted runners without paid SaaS or self-hosted infrastructure. Otherwise, document that it is out of scope.

### Locking and Persistent Cluster Hygiene

Before touching GCP, each run must acquire a per-target-cluster lock. In GitHub Actions this is a `concurrency` group such as `e2e-${cluster}` with `cancel-in-progress: false`, so runs queue instead of colliding. The runner should keep the lock until post-run cleanup and artifact collection finish.

Before each run:

- Verify cluster API and system pods are healthy.
- Clean CRD-backed resources first: NodeClaims, GCENodeClasses, then NodePools.
- Delete/recreate test namespaces and the Karpenter deployment state only after CRD cleanup has had a chance to process finalizers.
- Check for orphaned GCE instances/disks only within the configured project, location, and e2e ownership labels. Name prefixes are diagnostic hints, not sufficient deletion selectors.
- Re-run setup idempotently if infrastructure drift must be reconciled.
- Run quota checks before deciding parallelism.

Cleanup has a 5-minute normal timeout. If cleanup hangs, classify the run as infrastructure failure and point maintainers to manual cleanup. Force deletion may be used only by cleanup scripts with explicit logging and the same project/location/ownership-label guard; it must not hide finalizer bugs.

### Test Parallelism

Use two levels of parallelism:

1. **Suite-level:** run independent non-GPU suites concurrently.
2. **Spec-level:** run long suites such as provisioning with Ginkgo `--procs=N`.

The runner computes safe parallelism from current GCP quotas: addresses, SSD GB, regional CPUs, and all-regions CPUs. If full parallelism does not fit, run quota-safe waves instead of falling back to fully sequential execution.

Initial mode behavior:

- `standard`: non-GPU suites in suite-level parallel + provisioning with quota-safe `--procs=N`.
- `gpu`: GPU suite only, `E2E_GPU_TESTS=true`.
- `full`: standard and GPU as sub-runs; initially serial or quota-safe waves on the same cluster.

### Retries

Retries are intentionally limited:

- Transient setup/deploy operations may retry with backoff.
- Deploy alignment mismatch may redeploy once, then fail before tests.
- Quota exhaustion and GPU capacity failures do not loop; report them as infrastructure failures.
- Test assertion failures do not become green because of automatic reruns. A future flake-confirmation rerun may be added, but both attempts must be reported and the first failure must remain visible.
- Maintainers can always post the command again for a manual rerun.

### Reporting

Each run produces:

1. GitHub check on the tested SHA.
2. PR comment replacing the previous bot-authored result for that mode.
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

Adopt from the start:

- reusable workflow + portable runner scripts,
- GitHub OIDC/WIF cloud auth,
- SHA-pinned actions,
- env-var indirection for shell safety,
- per-cluster locking,
- failure diagnostics under `if: always()`,
- JUnit/JSON/log artifacts with 30-day retention,
- ownership-label-gated cleanup, with name prefixes used only as diagnostic hints.

Defer until needed:

- suite matrix expansion beyond `standard`/`gpu`/`full`,
- strict egress allowlist if it requires paid SaaS or self-hosted infrastructure,
- multi-project leasing.

We should not copy ephemeral cluster teardown from AWS/Azure because this proposal intentionally keeps GKE infrastructure alive between runs.

### KWOK

KWOK is useful for cheap, fast validation of provider-agnostic Karpenter behavior: scheduling logic, NodePool/NodeClaim controller interactions, disruption flows, and report/runner plumbing. It is not a replacement for this proposal's GKE e2e CI because it cannot validate GCP Compute API calls, GKE bootstrap metadata, IAM, images, networking, disks, GPUs, quota, or actual node registration.

KWOK should be included in this proposal as a later phase, not as a prerequisite for the first real GKE CI PR. A useful split is:

- **GKE lane**: maintainer-triggered, real GCP/GKE behavior, required for merge confidence.
- **KWOK lane**: cheap fast e2e for ready PRs or every trusted push, useful before spending GCP quota.

The KWOK lane can support issue #250 by covering portable scheduling/disruption scenarios early, while GCP-specific scenarios still run only in the GKE lane.

---

## Risks and Mitigations

| Risk                                        | Mitigation                                                                                              |
|---------------------------------------------|---------------------------------------------------------------------------------------------------------|
| Malicious PR burns quota or mines resources | maintainer-only trigger, dedicated project, quotas/budget alerts, timeouts, bounded parallelism/retries |
| Secret exfiltration                         | WIF short-lived credentials, least-privilege service account, no unrelated secrets in env               |
| Wrong commit tested                         | resolve SHA at trigger time; verify deployed image and embedded commit                                  |
| Persistent cluster leaks resources          | pre/post cleanup, labels/prefixes, diagnostics, manual maintenance runbook                              |
| GPU capacity unavailable                    | separate `gpu` mode, explicit capacity validation, infra-failure reporting                              |

---

## Coverage Roadmap

Use issue [#250](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/250) as the coverage backlog.

Suggested phasing:

1. **Infra first**: comment trigger, WIF, locking, cleanup, reports, and existing suites in GKE.
2. **KWOK fast lane**: provider-agnostic scheduling/disruption coverage on ready PRs without GCP credentials.
3. **Portable GKE parity**: scheduling, validation, consolidation, drift/hash, and expiration scenarios that do not need special GCP events.
4. **GCP-specific parity**: Spot termination notice handling, image drift, maintenance-event behavior, GPU, storage, networking, and arm64 coverage.

This keeps one proposal for the e2e program while allowing small phased PRs.

---

## Acceptance Criteria

- [ ] `/e2e-test`, `/e2e-test gpu`, and `/e2e-test full` can be triggered by maintainers and rejected for unauthorized users.
- [ ] The workflow checks out and tests an immutable PR head SHA.
- [ ] GitHub Actions authenticates to GCP through OIDC/WIF in steady state.
- [ ] Standard, GPU, and full modes initially target one persistent `asia-southeast1-b` cluster.
- [ ] GPU mode is enabled only after `nvidia-l4` and required ARM capacity are validated in `asia-southeast1-b`.
- [ ] The workflow verifies deploy/test alignment before running tests.
- [ ] Standard mode uses suite-level parallelism and quota-safe Ginkgo spec-level parallelism.
- [ ] Normal runs reuse infrastructure and do not perform teardown.
- [ ] Failures distinguish test failures from infrastructure/setup/quota/capacity failures.
- [ ] Reports include GitHub checks, Markdown PR comments, JUnit XML, JSON summaries, logs, and artifact links.
- [ ] Artifacts are retained for 30 days.
- [ ] Third-party actions are pinned by SHA and shell steps avoid direct unquoted GitHub-context interpolation.
- [ ] First-version CI and runner code lives in this repository.
- [ ] Documentation explains trigger syntax, permissions, modes, security model, troubleshooting, and maintenance.
- [ ] KWOK fast-lane design is documented before implementation and clearly separated from real GKE e2e.
- [ ] New coverage work references the #250 roadmap and identifies whether each scenario belongs in KWOK, GKE, or both.

---

## Implementation Phases

### Phase 1 — Comment Gate

Parse commands, verify actor permission, resolve SHA, post dry-run acknowledgements, and add parser/auth tests. No GCP access yet.

### Phase 2 — Standard Mode

Add WIF, persistent cluster setup, cleanup, deploy alignment checks, quota-aware standard-mode execution, artifacts, check runs, and PR reporting.

### Phase 3 — KWOK Fast Lane

Add cheap provider-agnostic e2e for ready PRs or trusted pushes. Use it for portable #250 coverage that does not require GCP APIs or real nodes.

### Phase 4 — GPU, Full Mode, and Coverage Expansion

Validate `asia-southeast1-b` GPU/ARM capacity, enable GPU mode, add full-mode aggregation, and start adding GKE parity suites from #250.

### Phase 5 — Hardening

Tune quota logic, formalize JSON schema, and document maintenance.

---

## Future Direction

- Multiple clusters for parallel runs.
- `/e2e-test retry` for maintainer-requested reruns.
- Periodic maintenance workflow for explicit cleanup or cluster rebuilds.

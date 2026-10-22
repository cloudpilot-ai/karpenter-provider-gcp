# Trusted manual e2e handoff

Build this CLI from the upstream workflow SHA, not the PR checkout:

```sh
go build -o /trusted/bin/e2e-ci ./hack/e2e-ci/cmd/e2e-ci
```

`.github/workflows/e2e.yaml` is the sole manual entry point: authorization → cloud-free pending check → credential-free PR build → owned runtime → trusted reporter, each on a fresh hosted worker. This CLI handles GitHub metadata/transport and publication; the existing runner owns lifecycle and offline rendering. Implementation is not activation. **Disabled by default**: unset/false `E2E_CI_ENABLED` or an empty `E2E_ENABLED_PRESETS` rejects every dispatch before build/cloud. Each preset needs separate operational approval and validation.

## Commands

`gate` reads `GITHUB_REPOSITORY`, `GITHUB_REF`, `GITHUB_WORKFLOW_REF`, `GITHUB_WORKFLOW_SHA`, `GITHUB_EVENT_NAME`, `GITHUB_ACTOR`, `GITHUB_ACTOR_ID`, `GITHUB_RUN_ID`, and `GITHUB_RUN_ATTEMPT` from the trusted Actions context. `GH_TOKEN` is used only for GitHub API requests. The CLI fixes the API origin.

```sh
e2e-ci gate --pr "$PR_NUMBER" --preset "$PRESET" \
  --enabled-presets "$ENABLED_PRESETS" > /trusted/gate.json
e2e-ci fetch --gate /trusted/gate.json --destination /build/pr
e2e-ci prepare --gate /trusted/gate.json --source /build/pr \
  --output /build/bundle.tar
```

Preserve gate JSON independently through trusted job outputs; **never recover it from a PR artifact**. The gate checks the actual upstream repository/default branch, Actions run, current actor permission and open upstream-main PR. It freezes the fork repository and full head SHA once. Reruns require a new dispatch. Fetch uses only the upstream PR ref and rejects movement/deletion instead of following a new contributor SHA. Build output must be outside the PR checkout.

Run fetch/preparation on a disposable worker with at most `contents: read`, no OIDC, cloud credentials, write token, environment secrets or unrelated secrets. Disable checkout credential persistence. Treat that worker as compromised. Preparation passes fixed argv and an explicit cwd; no PR Makefiles, shell scripts, local Actions, ko configuration, shared caches, credential stores or environment are inherited. It compiles rather than executes the unified tests and matching Ginkgo CLI. ko receives explicit `--platform=linux/amd64 --push=false --sbom=none`; only Go compilation receives `GOOS=linux GOARCH=amd64` (ko rejects both mechanisms together). Chart dependencies must have unchanged locks. Arbitrary PR compilation is not a sandbox: stripping child environments cannot protect secrets elsewhere on the worker.

Upload **only** `bundle.tar`, using name `e2e-bundle-<run_id>-1`. Capture the exact artifact ID and server SHA-256 digest from this upload's action outputs. Do not select an artifact by a mutable/latest name. On the trusted runtime worker, before WIF:

```sh
e2e-ci recheck --gate /trusted/gate.json --pr "$PR_NUMBER" --preset "$PRESET" \
  --enabled-presets "$ENABLED_PRESETS"
e2e-ci artifact --gate /trusted/gate.json --id "$ARTIFACT_ID" \
  --digest "$ARTIFACT_DIGEST" > /trusted/receipt.json
# Download that exact artifact ID as its original ZIP, not extracted/re-zipped.
e2e-ci verify --gate /trusted/gate.json --receipt /trusted/receipt.json \
  --zip /downloads/artifact.zip --parent /runtime/prepared
```

Recheck compares the current trusted invocation and rechecks repository, run and actor permission **without querying or refreshing the PR head**. API confirmation of the artifact requires `actions: read` on the trusted pre-auth runtime job; this additional read-only grant is approved, not granted to the build worker. The gate also reads Actions/PR metadata. Runtime has no GitHub writes. Scope `GH_TOKEN` to trusted verification/recheck calls, not PR execution environments or artifacts. Original artifact ZIP validation requires a download path that preserves the server's bytes; an extracted download action output is not that ZIP.

## Schema v1 and Go API

Import `github.com/cloudpilot-ai/karpenter-provider-gcp/hack/e2e-ci` (package `e2eci`); no package-main imports are needed.

- `Dispatch` + `Gate.Capture` produce `Frozen`; `Gate.Recheck` requires the exact current `Dispatch`. `Frozen` includes trusted workflow SHA, upstream repository, PR, fork repository ID/name, full head SHA, actor ID/login, run ID/attempt and preset.
- `Gate.Artifact` produces `Receipt` from exact ID/digest and server provenance. Keep this receipt separate from the downloaded artifact.
- `VerifyDownload(zipPath, parent, frozen, receipt)` verifies the server ZIP digest and then calls `VerifyBundle`. The ZIP contains exactly one regular `bundle.tar`.
- `VerifyBundle(reader, parent, frozen)` validates the tar and creates a private `Bundle.Root`; callers remove it. This lower-level API is for an already authenticated transport/offline fixtures, not a replacement for ZIP verification.
- `Bundle.ExtractControllerLayout(parent)` returns a separate private OCI directory and the single image digest for trusted registry tooling; callers remove it. It does not push. Installed `crane push` accepts OCI directories, unlike a Docker daemon tar assumption. No image upload has been exercised by this component.

The outer tar contains exactly:

| Filename             | Content                                                                                                                     |
|----------------------|-----------------------------------------------------------------------------------------------------------------------------|
| `manifest.json`      | Schema 1, exact `Frozen` source tuple, `linux/amd64`, Ginkgo v2 version, `success`, five file names/lengths/SHA-256 digests |
| `controller.oci.tar` | OCI layout 1.0.0, exactly one image, verified blob digests/descriptors, Linux/AMD64 config, full source SHA revision label  |
| `karpenter.tgz`      | Safely packaged controller chart, including unpacked locked dependencies                                                    |
| `karpenter-crd.tgz`  | Safely packaged CRD chart                                                                                                   |
| `e2e.test`           | Linux/AMD64 ELF precompiled Go test binary with matching Ginkgo dependency                                                  |
| `ginkgo`             | Linux/AMD64 ELF framework CLI with matching main/dependency module version                                                  |

No coordinator, reporter, argv, deployment target, shell configuration or additional files are accepted. Limits: 2 GiB bundle/download, 1 GiB per outer file, 64 KiB manifest; nested charts are limited to 128 MiB expanded/32 MiB per file, OCI metadata retention to 16 MiB, and nested archive entries to 20,000. Traversal, absolute/backslash paths, links/devices/directories, duplicate/unknown entries, PAX extensions, trailing archives, mismatched tuple/digests/platform/framework and incomplete bundles fail closed. OCI layer bytes are opaque image data, not extracted files. Checks prove transfer integrity/compatibility, **not honest or safe PR code**.

The controller's build version is the full captured SHA; Go VCS stamping remains enabled. Runtime still must verify the deployed image digest and embedded SHA. Use the prepared Ginkgo CLI's existing `.test` support, not `go run` or recompilation with credentials. Runtime chooses trusted argv/reports/target configuration and the owner-selected global worker cap of 20. Preserve the existing Go selections: `standard` excludes GPU and extended local SSD, `gpu` selects GPU, `full` maps to local `all` including both. This package deliberately defines no second selector.

## Offline checks

```sh
go test ./hack/e2e-ci/... -count=1
go test -race ./hack/e2e-ci/... -count=1
go vet ./hack/e2e-ci/...
```

Tests use HTTP/process boundaries and compile Linux/AMD64 bundle fixtures; runner tests execute tiny host-native fixtures. Workflow tests inspect the actual YAML permission/dependency/transport boundaries. None prove GitHub platform fork checks/artifact behavior, real cloud identity/refresh, or any live preset.

## Operator configuration (not applied by this change)

Repository variables are administrative configuration, **not dispatch inputs**:

| Variable                      | Required value                                                                               |
|-------------------------------|----------------------------------------------------------------------------------------------|
| `E2E_CI_ENABLED`              | Literal `true` only after separate activation approval; unset/false disables                 |
| `E2E_ENABLED_PRESETS`         | Comma-separated reviewed/validated `standard,gpu,full` subset; no spaces; empty disables all |
| `E2E_TARGET_CONFIG`           | JSON object from Terraform `ci_environment_variables`, exactly its twelve `E2E_*` entries    |
| `E2E_WIF_PROVIDER`            | Terraform `ci_workload_identity_provider`                                                    |
| `E2E_WIF_AUDIENCE`            | Terraform `ci_oidc_audience`, explicitly `https://iam.googleapis.com/` plus provider         |
| `E2E_RUNTIME_SERVICE_ACCOUNT` | Terraform `ci_runtime_service_account`                                                       |

Use the output names in `deploy/terraform-e2e/outputs.tf` (provider output is `ci_workload_identity_provider`). No private tfvars/state/env files belong in variables, summaries or artifacts. Target JSON explicitly includes project, region, location, prefix, cluster, repository, controller/node SAs, primary/small/ large pod-range names and tag-key **short_name**. No target/worker/shell/ref override is accepted. The gate validates and freezes this non-secret configuration with **20 global Ginkgo workers**. Runtime uses the frozen target, not queued-time variable edits; current enable/preset flags and actor permission are rechecked before WIF. Disable CI to revoke queued authorizations; stop active workers separately. A setting change is not active-worker cancellation.

Manually configure environment **`e2e-runtime`** to admit only upstream `main`, with **no required reviewers** and no unrelated secrets. Dispatch by a current upstream write/maintain/admin actor is the sole run/cost approval. The environment name alone does not enforce branch restrictions: missing operational restrictions block activation. Terraform CI identity defaults off and never configures GitHub. Review/bootstrap Kubernetes separately per the Terraform README; runtime does not repair namespaces, RBAC, ranges, tags, IAM or Lease state.

### Dispatch and selection

Dispatch this workflow on upstream `main` with a positive upstream PR number and one preset. Fork heads are allowed. The gate immediately displays captured head, fork/base identity, trusted SHA, actor, invocation and preset. PR pushes after capture never authorize a replacement SHA. A moved/deleted PR ref fails fetch; queue-time pushes do not change runtime's frozen source. Reruns, including "re-run failed jobs", are rejected: **new dispatch required**.

- `standard`: exact local standard, excluding GPU **and extended local SSD**.
- `gpu`: local GPU selection; explicit dispatch authorizes that enabled cost.
- `full`: exact local `all`, including GPU **and extended local SSD**.

All use **20**, not 20 per feature; no quota waves, automatic reduction, fallback zone or preset downgrade. Capacity review and live validation are separate for each preset, including standard. Do not enable unvalidated presets just because local fixtures pass.

### Separate activation gates

Before enabling anything, obtain owner approval for the exact IAM/WIF plan, transitive authority audit, bootstrap, environment/variables and candidate workflow staging. **First prove fork-head check creation with upstream `GITHUB_TOKEN` cloud-free in a separately approved workflow rehearsal.** Local HTTP fixtures and the pending job are not platform proof; do not activate WIF to find out whether checks work. Also prove original-ZIP/server-digest handoff from this invocation. No automatic rehearsal or alternate credential exists here. If fork checks fail, stop for an explicit reporting-contract decision; never use a merge SHA or broader PAT/App token as an implicit fallback.

Then separately prove accepted upstream-main/denied wrong-workflow/ref/fork OIDC contexts and actual refresh beyond both initial OIDC and impersonated token expiry for **gcloud, crane's gcloud credential helper, the GKE client-go plugin and Go ADC**. Auth creates a URL-backed refreshable `external_account` file; workflow moves it into a private runtime-only directory, actually runs `gcloud auth login --cred-file`, and isolates gcloud config/kubeconfig outside artifact/build roots. No static key or fixed access-token env is used. These configuration checks do not prove long-duration refresh. No workflow dispatch, settings change, apply, cloud mutation or live test was performed for offline implementation. Candidate rehearsal and post-merge upstream proof are distinct.

### Privileged execution, evidence and reporting

An authorized PR supplies controller, charts/CRDs and test binaries from the same captured SHA. This is **privileged arbitrary code in a dedicated project/cluster, not sandbox isolation**. Hashes/OCI/source checks prove transport/alignment, not honesty. The PR can use runtime-readable controller/node/workload authority and may lie or deliberately exfiltrate it. Audit inherited IAM, metadata, SA keys, Kubernetes escalation/admission, network reach and state/maintenance/unrelated systems before activation. Such transitive reach blocks activation; minimal direct runtime IAM alone is insufficient. Do not attach unrelated secrets or use this target for other workloads.

The build worker has no OIDC, cloud credentials, environment or write token; checkout credentials and shared caches are disabled. Runtime has read-only GitHub API access, no GitHub writes; trusted reporter has checks/PR-comment writes but no OIDC/cloud or PR execution. Actions are exact-SHA pinned. Runtime confirms upload ID/name/server digest/run/source, downloads **original ZIP bytes**, and validates bounded ZIP/tar/OCI/chart/Go-binary compatibility before WIF.

Private native JSON/JUnit, logs and lifecycle evidence are not upload allowlists. `e2e-runner export` validates source/schema/counts and projects onto native-schema JSON/JUnit with anonymous `spec-NNNNN` IDs, known feature labels, enum states, bounded durations, numeric resource samples and numeric controller severity counts. Arbitrary spec names, paths, failure messages, stdout/stderr, report entries, raw suite/controller logs and image repository identifiers are omitted; unsafe diagnostics are explicitly unavailable. Original native evidence remains private on the ephemeral worker. Numeric projection/redaction does not sandbox malicious authorized code or guarantee absence of deliberate covert leakage.

Upload only named public projection files for **30 days**. Never upload raw logs, credentials, kubeconfig, environment dumps, Terraform state/tfvars, auth ZIPs or a whole workspace/build/results tree. The OCI bundle alone is retained one day on the credential-free build worker. Reporter downloads exact same-run safe artifact ID/digest, rejects links/traversal/unknown/oversized entries, **reprojects** it on a fresh worker and renders using the existing trusted runner template. Artifact Markdown/templates/binaries are never executed or published. Reports are escaped and bounded; omitted rows are explicit. Missing/corrupt/zero-test/unfinished reports, failed build/runtime/cleanup, cancellation and failed artifact upload cannot pass. Upload and publication outcomes are separate from test outcomes.

Pending/final checks target exactly the gate's head SHA. A later PR head does not retarget the check; stale-head comments are suppressed. Only `github-actions[bot]` Bot comments with this preset's exact marker may be updated; user comments remain untouched. Reporter publication is serialized per PR/preset and compares run IDs against bot comments and same-SHA checks while holding that lock. Older invocations cannot overwrite newer results, even if a newer check's comment failed. Each invocation's own check is still finalized where possible. Check/comment API errors are attempted independently and retained in the 30-day delivery artifact and job summary, rather than changing test results or claiming publication success.

### Failure recovery and maintenance

Graceful cancellation kills test child groups while Lease renewal continues through bounded owned cleanup under confirmed ownership. Lease loss/uncertainty stops mutation, including cleanup/release. Unknown resources, incomplete drain, abandoned holder or maintenance marker block the target; do not automatically force-unlock, remove finalizers or delete cloud resources. Forced host death has no cleanup guarantee. See `hack/e2e-runner/README.md` and the Terraform root README for the exact owned namespace/RoleBinding/Lease and maintenance exclusion contract. Supported local deploy/clean commands use the same Lease. Standalone `e2e-test` bypasses it and is **unsupported on the shared target**.

Before maintenance: disable CI/presets, stop **all** hosted/local workers and establish administrator-reviewed exclusion, then investigate actual ownership, cloud deletion and bootstrap. During API outages, inability to see a holder is not permission to override it. Terraform state locking is not this runtime lock.

Cancellation can prevent the always-run exporter/reporter itself from starting; GitHub publication concurrency is not FIFO and may cancel pending jobs. Pending checks may therefore remain incomplete. Maintainers must inspect the invocation, confirm workers have stopped/cleanup state, then reconcile only that exact invocation/SHA check as non-success using separately authorized GitHub access. Never infer pass from missing artifacts, an incomplete check or a newer comment. Use a new dispatch after safe recovery; do not rerun an old authorization.

`make e2e-offline-test` wires helper, runner, identity/bootstrap and environment regressions into `make ci`/presubmit. Full presubmit/fresh review is a separate integration gate; live standard/gpu/full and refresh/cleanup remain unproven until separately approved and observed.

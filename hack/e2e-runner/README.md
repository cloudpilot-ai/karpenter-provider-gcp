# E2E runner

The default CLI remains the local, Lease-protected, test-only runner:

```sh
go run ./hack/e2e-runner --selection standard --report .pi/results.md
```

`--preset` remains an alias; feature-directory selections and extra Ginkgo arguments remain supported. `standard` excludes GPU and extended local SSD; `all` includes both. Empty selections fail. With `--report`, the runner retains native JSON and JUnit in a fresh, private `<report-path>-native-*` directory beside the Markdown report. These files and controller logs are **private evidence**, not safe public artifacts.

## Trusted prepared lifecycle

Opt-in CI entry point (build the coordinator from trusted upstream workflow source):

```sh
/trusted/tools/e2e-runner prepared --source /trusted/gate.json \
  --receipt /trusted/receipt.json --download /private/original-artifact.zip \
  --results-dir /private/new-results --lock-id upstream-run-123
```

Integration **must** verify the original ZIP before WIF and recheck authorization with the trusted gate. This command verifies the same download again after authentication; it does not authorize a dispatch or replace that pre-auth gate. No prepared controller/chart/test compilation or PR shell script runs here. Runtime requires trusted `kubectl`, `gcloud`, `helm`, `crane`, and `docker-credential-gcloud` on PATH. Credentials/kubeconfig/config directories belong outside bundle/results roots. Registry upload uses a temporary private Docker config with the gcloud credential helper, never an access-token environment variable or the operator's Docker config. Real refresh across credential expiration remains a live activation gate.

All CI target values are explicit; no CI defaults/target flags:

- `E2E_PROJECT_ID`, `E2E_LOCATION`, `E2E_REGION`, `E2E_PREFIX`, `E2E_CLUSTER_NAME`.
- `E2E_IMAGE_REPOSITORY` (regional Artifact Registry image path in that project).
- `E2E_CONTROLLER_SERVICE_ACCOUNT`, `E2E_NODE_SERVICE_ACCOUNT` (same project).
- `E2E_PODS_RANGE_NAME`, `E2E_SMALL_PODS_RANGE_NAME`, `E2E_LARGE_PODS_RANGE_NAME`.
- `E2E_RESOURCE_MANAGER_TAG_KEY`: **short_name**, not `tagKeys/id`; tests construct `project/short_name` and expect the existing `e2e` tag value.

The coordinator checks the kubeconfig context **and actual GKE endpoint/identity**, existing controller namespace/priority class/Lease, reviewed owned test namespace and its exact runtime RoleBinding, actual primary/additional subnet range membership and small/large CIDR capacity semantics, and existing tag key/value. It never repairs bootstrap or creates ranges. Namespace/deployment overrides are forbidden in CI. Test environment target values are explicitly projected from this frozen configuration, not ambient test overrides.

One `karpenter-e2e-tests` Lease covers old-controller drain, prepared OCI push and remote digest confirmation, both packaged Helm installs, rollout/digest/full-SHA verification, matching native Ginkgo execution with exactly **20** workers, bounded diagnostics, cleanup and persistence. The existing test namespace must have `karpenter-e2e/owned=true` and a UID. Its `e2e-runtime-test-workloads` RoleBinding must reference the same-named ClusterRole and only the dedicated `${prefix}-ci-sa@${project}.iam.gserviceaccount.com` User. Missing/mismatched/terminating bootstrap fails before image push. Namespace and binding survive every drain; the coordinator never creates, adopts, relabels or deletes them. Current suite initialization GETs an existing namespace; only local NotFound may create one. CI deliberately lacks Namespace create/delete permissions.

Cleanup accepts only labeled Karpenter resources and label-owned Deployment roots in the preserved namespace. ReplicaSets/Pods require matching immutable UID controller-owner references back to those roots; unproven workloads fail before any deletion. Deployment deletion is label/name-filtered, foreground and bounded; final inventory must contain no workloads. Existing suite helpers can create **unlabeled Deployments**: their normal cleanup remains responsible; leftovers require maintenance, never inferred ownership from namespace/name/selector. The bootstrap namespace UID is verified unchanged. Before deleting, it correlates VMs to owned NodePools/NodeClasses/NodeClaim `gce://project/zone/name` provider IDs, cluster/location labels, immutable Compute IDs/selfLinks and attached disk identities. Only actual GKE node-pool MIG metadata proves bootstrap VM/disk exclusion. Unknown project VMs/disks or legacy/unlabeled resources fail closed. Kubernetes finalizers/old controller delete cloud resources; the coordinator never strips finalizers or directly deletes cloud objects. It verifies captured full VM/disk identities disappear, rejects replacement IDs, and fails on incomplete drain. Diagnostics are bounded private `controller.log`; no raw logs/results are public-safe.

Test cancellation stops native process groups but keeps a separate heartbeat alive through at most ten minutes of cleanup. Lost/stolen/expired Lease or any ownership/renewal uncertainty cancels all mutating child contexts immediately when detected; only read-only diagnostics/local evidence writes remain allowed. There is no host-death cleanup guarantee. Local cleanup cancellation terminates the shell process group without retrying cleanup and marks maintenance only while ownership remains confirmed. Terminating a CLI does not cancel cloud operations already accepted by the server.

## Local shared-target commands and maintenance

`make e2e-deploy` / `hack/e2e-deploy.sh` and `make e2e-clean-env` / `hack/e2e-clean-env.sh` route through this same coordinator/Lease. `E2E_LOCK_ID` is forwarded by `make e2e-tests`; deploy/cleanup accept `--lock-id` when invoked directly. Explicit local deploy keeps local ko build with `e2e-SHA` tags and published `RELEASE_VERSION` modes. The original Bash scripts define sourceable cleanup/deploy phases; sourcing is inert, and only their standalone entry points acquire the Lease through Go. Under that single Lease, Go captures shared read-only ownership/cloud inventory, invokes Bash cleanup, proves captured VMs/disks and resources disappeared, then invokes Bash deployment. Bash owns all local deletion/build/push/Helm mutations; prepared CI retains its strict Go lifecycle and never enters those phases. There is no nested cleanup lock. All commands use the canonical `karpenter-system/karpenter-e2e-tests` Lease; local controller namespace overrides never select a separate lock. Local-only target derivation preserves the prefix default and derives a missing region from the required regional/zonal location. The unchanged Make deploy recipe still defaults `E2E_REGION` to `us-central1`, so other-region deploys must set it explicitly; Make cleanup does not forward it. Prepared CI requires explicit region and all other target configuration. Deploy/clean require existing controller namespace/Lease. CI additionally requires the reviewed owned test namespace/RoleBinding bootstrap; explicit local cleanup on default-off manual targets permits an absent test namespace and does not require a CI identity/binding. Any existing namespace must still prove ownership and is preserved. Normal coordinator commands never create or repair them. See `deploy/terraform-e2e/ci-bootstrap-rbac.yaml` for the separately reviewed manual example. The default runner is still test-only and does **not** deploy. Standalone `make e2e-test` bypasses the Lease and is **unsupported on the shared CI target**.

All acquisitions reject abandoned nonempty expired Leases and `karpenter.k8s.gcp/maintenance-required`. Incomplete/unknown cleanup sets the sticky marker to bounded enum `incomplete-drain` through owner-token/resourceVersion CAS, then releases the holder without clearing the marker. Uncertain ownership never writes a marker or releases the Lease. Private outcomes record `maintenance_required`; cleanup/lease-release failures never overwrite primary test results or become success.

Only an explicit owner-approved maintenance exclusion may clear an abandoned holder/marker: disable CI administratively, stop all workers and local commands, verify processes/controllers/resources are stopped or safely drained, and investigate cloud identities before altering the Lease. During API outages ownership/cleanup cannot be proven: keep CI disabled and workers stopped; do not force-delete the Lease or use unlocked scripts. Normal CLI paths never clear markers. Terraform state locking is unrelated to this runtime exclusion. Each preset still needs separate live enable/validation.

## Prepared test execution seam

`executePrepared(ctx, preparedExecution)` is an internal, **lock-held test executor**, not a complete CI entry point. It does not acquire/release the Lease, deploy, clean resources, authenticate, repair infrastructure or publish reports. The lifecycle coordinator must:

1. Build itself and this renderer separately from the trusted workflow SHA, never from PR artifacts. Keep trusted gate/receipt metadata separate from the PR bundle.
2. Use `e2eci.VerifyDownload` on the original artifact ZIP before cloud authentication, and recheck authorization and target configuration.
3. Acquire the shared Lease before any push/deploy/cleanup; deploy the validated bundle with trusted tooling under that same ownership.
4. Pass the validated `Bundle`, independent `Frozen`, trusted image repository plus the bundle's exact OCI image digest, explicit `Workers: 20`, and a **nonexistent** output directory under a private parent outside credentials and bundle roots.
5. Retain ownership through diagnostics, cleanup and final result persistence. On ownership loss, cancel the execution context (terminates the child process group), stop mutation, and record maintenance-required evidence. This executor does not provide ownership-aware cleanup.

The executor rechecks all five bundle-file hashes and the OCI source/digest; checks rollout, desired image, every ready replica's running image digest and full embedded controller SHA; then runs the prepared matching Ginkgo CLI on the native `.test` binary. No runtime git identity, compilation, arbitrary Ginkgo argument overrides or artifact-selected worker count is used. CI presets reuse local selection: `standard`, `gpu`, and `full` → `all` (including GPU and extended SSD). All presets still need separate administrative activation/live validation.

The subprocess environment is allowlisted for target configuration, kubeconfig, refreshable ADC/gcloud configuration, and existing optional suite flags. It does not inherit the GitHub token. This is privileged execution of authorized PR code, **not a sandbox**; files readable by that code must not contain unrelated secrets.

## Private outcome schema and durable evidence

`outcome.go:runOutcome` defines schema 1. `outcome.json` records the frozen source/invocation, requested/resolved selection, workers, timestamps, image/controller identity, counts, and independent `infrastructure`, `tests`, `reports`, `cleanup`, `diagnostics`, `artifact`, `publication` states. States are `not-run`, `running`, `passed`, `failed`, `cancelled`, `incomplete`, `unavailable`. There are no raw error strings in this schema.

`Overall()` requires successful infrastructure, nonzero tests, valid native reports, finished timestamps and successful cleanup with no maintenance requirement. Delivery failures remain separate from test outcomes. The lifecycle owner must update cleanup/diagnostics states and call `persistOutcome` then `renderPreparedReport` before release; delivery owners record upload/publication independently. A crash can leave `running` with no finish timestamp; that is incomplete, never success. Failure to create/write the evidence directory is returned as an error; persistence is not guaranteed after host death or disk failure.

Each prepared run retains:

- `results.json` and `junit.xml`: native, private Ginkgo evidence; neither is deleted.
- `suite.log`: private stdout/stderr, bounded to 32 MiB; overflow fails execution.
- `outcome.json`: atomic, private structured lifecycle evidence.
- `report.md`: atomic output from the existing trusted formatter, including incomplete/failure outcomes.

Readers reject missing/corrupt/linked/oversized reports, trailing XML, zero executed specs, and inconsistent JSON/JUnit counts/states. Each native report is limited to 16 MiB, JSON to 64 suites/20,000 spec entries; JUnit to 20,000 cases. These are reader/export bounds, not filesystem isolation against malicious PR code.

## Offline trusted rendering

Build **from the independent trusted checkout**, not the PR build worker:

```sh
go build -o /trusted/tools/e2e-runner ./hack/e2e-runner
/trusted/tools/e2e-runner render --source /trusted/gate.json --results-dir /private/results
```

Rendering uses no git, kubectl, Go compilation, cloud API or PR executable/template. Gate attribution must exactly match the bounded strict outcome schema; successful outcomes also require consistent native JSON/JUnit and successful native suites. Invalid evidence removes any stale/PR-supplied `report.md` and fails closed. Explicit failed/incomplete outcomes can render without nonexistent native results.

The **same** `report.go`/`report.gotmpl` formatter serves local and offline paths. Text is control/bidi-stripped, punctuation entity-escaped and truncated to 128 visible characters; summaries show at most 32 feature rows and 64 spec rows, with omission notices. Output is capped at 60,000 bytes; overflow returns an error rather than a publishable partial report. Excluded/unexecuted features are not called passing.

Escaping is not secret redaction. Neither native reports, logs, private outcome metadata nor rendered spec names are automatically public-upload safe. The separate reporter/export step must schema-allowlist and sanitize its public artifacts before upload; it must not execute PR templates or trust a preexisting Markdown file. Safe artifact export and GitHub publication remain separate integration responsibilities.

## Offline regression checks

The unchanged upstream Makefile does **not** include the offline pipeline/security tests in `make ci` or `make presubmit`. Run them explicitly from the repository root:

```sh
go test -race ./hack/e2e-ci/... ./hack/e2e-runner ./deploy/terraform-e2e ./test/pkg/environment -count=1 -timeout=300s
```

These use local process/API fixtures, not a live cloud target. Live cleanup, IAM and credential refresh remain separate activation gates.

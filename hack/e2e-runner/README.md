# E2E runner

The default CLI remains the local, Lease-protected, test-only runner:

```sh
go run ./hack/e2e-runner --selection standard --report .pi/results.md
```

`--preset` remains an alias; feature-directory selections and extra Ginkgo arguments remain supported. `standard` excludes GPU and extended local SSD; `all` includes both. Empty selections fail. With `--report`, native JSON/JUnit remain in a fresh adjacent private directory. The local Markdown formatter bounds and escapes text, but neither reports nor controller logs are public-safe.

## Manual PR workflow

[`.github/workflows/e2e.yaml`](../../.github/workflows/e2e.yaml) is the sole CI entry point: trusted gate → pending frozen-SHA check → credential-free build → runtime → independent reporter. It is **disabled by default**. Configure identity, target variables, environment restrictions and bootstrap only after the [separate activation gates](../../deploy/terraform-e2e/README.md#opt-in-manual-ci-identity-not-activated). There is no ChatOps trigger.

An upstream write/maintain/admin actor dispatches on upstream `main` with a positive upstream PR number and one separately enabled preset. Fork heads are allowed. The gate captures the full head SHA once; build fetches that upstream PR ref and rejects movement/deletion. Later pushes never retarget runtime or checks. Reruns are rejected: use a **new dispatch**. `standard` excludes GPU and extended local SSD; `gpu` selects GPU; `full` is local `all`, including both. All use **20 global workers**, without automatic downgrade or quota waves. Local fixtures do not validate capacity or authorize any preset.

### Five raw build artifacts

The isolated build worker has no OIDC, cloud credentials, environment secrets or GitHub writes. Trusted fixed ko/Helm/Go commands compile the frozen PR without running PR scripts or Makefiles. It prepares exactly `controller-image.tar`, `controller-chart.tgz`, `crd-chart.tgz`, `e2e.test` and matching `ginkgo`. Pinned **upload-artifact v7** uploads each file separately with `archive: false`, its basename as artifact name and **one-day retention**, capturing exact IDs/digests. No custom bundle, manifest or receipt is used.

Before WIF, trusted Octokit checks five distinct positive IDs, exact names, same-run/repository provenance, matching nonempty server digests and bounded sizes. Pinned **download-artifact v8** downloads each exact ID into its own fresh directory with `skip-decompress: true` and `digest-mismatch: error`. A second gate requires all five regular nonsymlink files, exact sizes and no unexpected paths: the Action alone can warn on partially missing requested IDs. Standard `crane validate --tarball`, Helm and Go build metadata checks also run before WIF. After authentication the fixed trusted shell uses `crane push` on that ko tarball; Helm parses the packaged charts. There is no custom archive parser or original-ZIP downloader.

SHA/digest/provenance checks establish alignment and transfer integrity, **not honest PR code**. This is privileged authorized controller/chart/test execution in a dedicated project, not sandbox isolation. Audit transitive controller/node/workload authority before activation; never expose unrelated secrets. Runtime has no GitHub writes; reporter has no cloud/OIDC credentials and executes no PR binaries. Trusted Actions are SHA-pinned; refresh beyond initial OIDC and impersonated-token expiry still needs live proof for gcloud, crane's credential helper, GKE client-go and Go ADC.

### Public evidence and recovery

The small trusted standard-library projector, [`.github/scripts/e2e/project.py`](../../.github/scripts/e2e/project.py), bounds private native Ginkgo JSON and the three-field runtime verdict, comparing the independent expected SHA/preset. It retains numeric counts/timing, known feature labels, enum states and generated safe spec IDs only. Arbitrary names, paths, failures, output and image identifiers are stripped. Public JUnit is generated from that same safe data, not a second hostile document. This is sanitization, not protection against malicious authorized code or deliberate covert leakage.

Only **sanitized native-format JSON/JUnit**, plus a small `summary.json`, are uploaded for **30 days**. Summary is a single raw-file artifact; `sanitized-native-reports` uses the standard Action archive for the two sanitized reports. Raw reports/logs and credentials remain private on ephemeral workers; public log evidence is marked unavailable. Never upload whole result/workspace trees, credentials, kubeconfig, environment dumps or Terraform state/inputs. Rich lifecycle/resource/log reporting is deferred, not replaced with a new reporting framework.

The reporter verifies/downloads the exact raw summary ID/digest and validates its bounded fields. Missing/corrupt/zero-test evidence, build/runtime/cleanup/projection/upload/download failure and cancellation cannot pass. It combines independent job outcomes with evidence, finalizes its own frozen-SHA check independently of comment errors, then updates only a matching bot-owned PR/preset comment. Publication is serialized per PR/preset with newer-run protection; moved-head comments are suppressed. There is no public export/render CLI or delivery-artifact protocol.

Cancellation may prevent projection/reporter jobs from starting; publication concurrency is not FIFO and pending jobs can be cancelled. Checks can remain incomplete. Confirm workers stopped and inspect cleanup/maintenance state before separately authorized reconciliation of only that invocation/SHA as non-success. Missing evidence or a newer comment never implies pass. Recover the target safely, then dispatch anew, never rerun an old authorization. The Lease/maintenance rules below apply equally to local and CI mutators; Terraform state locking is not runtime exclusion.

Activation still requires cloud-free **fork-head check publication and exact raw-file Action handoff** proof, positive upstream-main/negative wrong-ref/workflow/event/fork WIF exchanges, long-duration credential refresh, and each preset's capacity/cleanup/cancellation/contention proof. Candidate rehearsal and post-merge upstream proof require separate approval. Offline checks do not establish any of these live properties.

## Trusted prepared command

Build the coordinator from the trusted workflow checkout. Run from that checkout:

```sh
e2e-runner prepared --source-sha <full-sha> --selection standard \
  --controller-archive /private/artifacts/controller-image.tar \
  --controller-chart /private/artifacts/controller-chart.tgz \
  --crd-chart /private/artifacts/crd-chart.tgz \
  --test-binary /private/artifacts/e2e.test \
  --ginkgo-binary /private/artifacts/ginkgo \
  --results-dir /private/new-results --lock-id upstream-run-123
```

All nine flags are required. Selection is `standard`, `gpu`, or `full` (local `all`). Workers are fixed at **20**, with fail-on-empty. The workflow owns authorization/rechecks, five raw-file provenance/digest/completeness checks and standard-tool format/build metadata validation **before cloud authentication**. The runner does not parse image/chart archives, authorize GitHub dispatches, compile PR code or execute PR deployment scripts.

All target variables are explicit, without CI derivation:

- `E2E_PROJECT_ID`, `E2E_LOCATION`, `E2E_REGION`, `E2E_PREFIX`, `E2E_CLUSTER_NAME`.
- `E2E_IMAGE_REPOSITORY` (regional Artifact Registry image path in that project).
- `E2E_CONTROLLER_SERVICE_ACCOUNT`, `E2E_NODE_SERVICE_ACCOUNT` (same project).
- `E2E_PODS_RANGE_NAME`, `E2E_SMALL_PODS_RANGE_NAME`, `E2E_LARGE_PODS_RANGE_NAME`.
- `E2E_RESOURCE_MANAGER_TAG_KEY`: short name, not `tagKeys/id`; the existing `e2e` value is required.

`validate-target` checks this configuration without cloud calls and remains available before authentication. Runtime preflight checks kubeconfig context and actual GKE endpoint/identity, controller namespace/priority class, reviewed test namespace/RoleBinding, subnet range attachment/capacity and tag configuration. Controller namespace/deployment overrides are forbidden. Missing bootstrap is never repaired.

One canonical `karpenter-system/karpenter-e2e-tests` Lease covers capture, original sourceable Bash cleanup, disappearance proof, fixed `.github/scripts/e2e/runtime.sh deploy`, controller verification, native tests, diagnostics and post-cleanup/proof. Only that trusted shell owns crane push and prepared Helm deployment. It receives explicit target environment, uses an ephemeral refreshable gcloud registry helper and records one digest-pinned reference in `controller-image.txt`. The runner requires the expected repository/digest, desired deployment image, every ready replica's running digest and full embedded source SHA before tests.

Artifact directories, new private results and credential/kubeconfig/gcloud configuration paths must not overlap (including symlink aliases). Temporary registry credentials cannot be placed inside artifacts/results. Subprocess environment is allowlisted, not inherited GitHub tokens or compiler configuration. This executes authorized privileged PR tests, **not a sandbox**; do not expose unrelated secrets.

## Ownership, cleanup and maintenance

Cleanup accepts only label-owned Karpenter resources and Deployment roots. ReplicaSets/Pods require immutable UID controller-owner references to those roots. Namespace ownership/UID and the exact dedicated runtime RoleBinding must already exist; they survive cleanup. Current suites can create unlabeled Deployments: leftovers require maintenance, never name/namespace-based adoption.

Read-only inventory correlates VMs with owned NodePools/NodeClasses/NodeClaim provider IDs, target cluster/location labels, immutable Compute IDs/selfLinks and attached disks. Only actual GKE node-pool MIG metadata excludes bootstrap VMs/disks. Unknown VMs/disks/resources block mutation. The original Bash cleanup deletes owned Kubernetes roots; finalizers/the old controller delete cloud resources. No finalizer stripping or direct cloud deletion is used. Captured VMs/disks must disappear; replacement IDs, remaining/new resources or changed bootstrap identity fail closed before deployment.

Cancellation kills subprocess groups while an independent Lease heartbeat permits at most ten minutes of post-cleanup. Lost/stolen/expired or uncertain ownership cancels mutating contexts; only bounded read-only diagnostics and local evidence writes remain allowed. Incomplete/unknown cleanup marks `karpenter.k8s.gcp/maintenance-required=incomplete-drain` only under confirmed ownership. Release never clears that marker. Every acquisition rejects maintenance markers and abandoned nonempty expired holders.

Only an owner-approved maintenance exclusion may clear an abandoned holder/marker: disable CI, stop workers/local commands and investigate/drain cloud resources first. API outages are not permission to force-delete Leases or run unlocked scripts. Host death has no cleanup guarantee; terminating processes cannot cancel cloud operations already accepted by the server. Presets and credential-refresh behavior still require separately authorized live activation proof.

## Private evidence

The runner writes `runtime.json` once finally, with exactly:

```json
{"source_sha":"<full-sha>","test_exit":0,"cleanup_ok":true}
```

`test_exit` is null when tests did not start, otherwise 0..255 (signal termination is nonzero). `cleanup_ok` requires confirmed post-cleanup/proof. Cleanup, persistence and Lease-release errors also fail the command independently of passing test data. A missing verdict after a crash cannot pass reporting.

Other private files are native `results.json`/`junit.xml`, `suite.log` (32 MiB cap), `controller.log` (bounded diagnostics) and `controller-image.txt`. Native JSON is the authoritative test evidence; missing/corrupt/symlink/oversized JSON, zero executed tests, failed suites and incomplete spec states cannot pass. The JSON reader bounds input to 16 MiB, 20,000 spec entries and 64 suites. Private JUnit is still generated by Ginkgo but is not parsed or cross-compared; public JUnit is generated from the same sanitized JSON projection. Workflow projection has its own tighter public bounds and emits sanitized native-format evidence; raw logs/reports stay private. There is no runner export/render CLI or lifecycle-state schema. The pre-existing local Markdown renderer remains available through `--report`.

## Local shared-target commands

`hack/e2e-deploy.sh` and `hack/e2e-clean-env.sh` retain their sourceable Bash phases and standalone canonical-Lease coordination. Local deployment retains ko `e2e-SHA` tags and published `RELEASE_VERSION` modes, without nested locks. Local-only target derivation retains the prefix default and derives a missing region from location. Non-US local deploys must set region explicitly because the unchanged Make recipe defaults it. Local cleanup permits an absent test namespace without CI-only identity bootstrap, but any existing namespace must prove ownership and is preserved. The default runner does not deploy. Standalone `make e2e-test` bypasses locking and is unsupported on the shared CI target.

## Offline regression checks

The unchanged Makefile does not wire these runner/security fixtures into presubmit. Run explicitly from repository root:

```sh
node --test .github/scripts/e2e/*_test.cjs
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s .github/scripts/e2e -p '*_test.py'
go test -race ./hack/e2e-runner ./deploy/terraform-e2e ./test/pkg/environment -count=1 -timeout=300s
go vet ./hack/e2e-runner
deadcode -filter='github.com/cloudpilot-ai/karpenter-provider-gcp/hack/e2e-runner' ./hack/e2e-runner
```

Fixtures use real precompiled Ginkgo/native reports, original trusted Bash phases and local process/API boundaries, never a live cloud target.

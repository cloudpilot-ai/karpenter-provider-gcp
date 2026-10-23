# Persistent e2e environment

Shared configuration is committed in `terraform.tfvars`:

- Project: `planetscale-karp-gcp-e2e`
- Regional control plane: `asia-southeast1`
- Worker zones: `asia-southeast1-a`, `asia-southeast1-b`, `asia-southeast1-c`
- Prefix: `karpenter-e2e`; resource names follow the established e2e naming
- System pool: `default-pool`, one `n2-standard-2` node in `asia-southeast1-b` with a 30-GB boot disk
- Release channel: `STABLE`; GKE selects and auto-upgrades Kubernetes versions

Private environments override these values and use their own state; see [Private environments](#private-environments).
- Datapath: Dataplane V2 (`ADVANCED_DATAPATH`), so Cilium/eBPF routes Services and NetworkPolicy is always enforced
- Telemetry: all GKE-managed logging, monitoring and managed Prometheus components are disabled

The [installation module](../terraform/README.md) creates the network, cluster, system pool and controller/node identities. This root adds NAT, an image registry and test tag fixtures. Controller permissions come from `deploy/iam/karpenter-controller-role.yaml`. APIs and the state bucket must already exist. Existing infrastructure is not imported automatically.

## Shared state

The GCS backend uses bucket `planetscale-karp-gcp-e2e-tfstate` in the same project, with prefix `karpenter/e2e`. Use this backend and the default Terraform workspace so maintainers share one state. The bucket is managed separately; use versioning, uniform bucket-level access and public-access prevention. Keep state locking enabled.

Authenticate with Application Default Credentials. Do not commit credentials, state, saved plans or local overrides; committed configuration is non-secret. From this directory, initialize the existing backend:

```sh
terraform init -input=false -lockfile=readonly
```

Variables come from `terraform.tfvars` automatically, but backends cannot read variables: a private environment overrides the state location at init time instead (see below).

Review a plan before applying. Bucket creation, imports, state migration, infrastructure changes and live e2e runs require separate approval. Coordinate maintenance with other users: Terraform state locking does not block tests or deployments.

The shared project already contains a `karpenter_controller` custom role. If Terraform reports that it already exists, adopt it instead of deleting it:

```sh
terraform import 'module.cluster.google_project_iam_custom_role.karpenter_controller' \
  projects/planetscale-karp-gcp-e2e/roles/karpenter_controller
```

## Private environments

Maintainers can run their own copy of this environment in their own project, so CI costs stay on the sponsored project. The committed `terraform.tfvars` targets the shared cluster; a private environment overrides it and uses its own state.

1. Create a gitignored `private.local.auto.tfvars`. Terraform loads `*.auto.tfvars` after `terraform.tfvars`, so these values win:

   ```hcl
   project_id            = "my-project"
   region                = "us-east5"
   location              = "us-east5"
   node_locations        = ["us-east5-a", "us-east5-b", "us-east5-c"]
   system_node_locations = ["us-east5-a"]
   prefix                = "karpenter-e2e-my-e5"
   controller_role_id    = "karpenter_e2e_my_e5_controller"

   additional_pod_ranges = {
     karpenter-e2e-my-e5-pods-small = "10.13.0.0/21"
     karpenter-e2e-my-e5-pods-large = "10.14.0.0/19"
   }
   ```

2. Choose where the private state lives. Either keep it local with a gitignored override file, then `terraform init -reconfigure`:

   ```hcl
   terraform {
     backend "local" {
       path = "/path/to/karpenter-e2e-my-e5.tfstate"
     }
   }
   ```

   Or use your own bucket. A `-backend-config` file overrides the committed bucket and prefix, and Terraform's `*.gcs.tfbackend` naming is already gitignored:

   ```sh
   gcloud storage buckets create gs://my-e2e-tfstate --project=my-project --location=us-east5 \
     --uniform-bucket-level-access --public-access-prevention
   printf 'bucket = "my-e2e-tfstate"\nprefix = "karpenter/e2e-my-e5"\n' > private.gcs.tfbackend
   terraform init -reconfigure -backend-config=private.gcs.tfbackend
   ```

3. Keep environments apart: pointing `project_id` at the sponsored project requires `prefix = "karpenter-e2e"`, every other project needs its own prefix, and `controller_role_id` must be unique within a project because custom role IDs are project-scoped. The system pool's zones must also be part of `node_locations`.

Telemetry stays off unless `cloud_observability_enabled = true`. Run tests against your cluster with your own kubeconfig and the same `E2E_*` variables listed above, taking `E2E_NODE_SERVICE_ACCOUNT` from `terraform output -raw node_service_account`.

## Offline validation

These commands do not access the shared state or apply infrastructure:

```sh
terraform fmt -check -recursive
terraform init -backend=false -input=false -lockfile=readonly
terraform validate
terraform -chdir=../terraform init -backend=false -input=false
terraform -chdir=../terraform validate
```

## Test fixtures and runner

- **Pod ranges:** the committed small `/22` and large `/20` ranges are attached to GKE for multi-range networking tests, the sizes the retired setup script used. The ranking test needs both ranges to report at least 1,024 free IPs and the large range to be richer; the small range meets that floor only while it is unallocated, which the serial networking specs ensure by releasing their nodes before the check. A free-IP status refresh can still delay the check regardless of range size. Changing the prefix requires updating range names in `terraform.tfvars`.
- **Resource Manager tags:** `${prefix}-tag` and its `e2e` value let tests verify `spec.resourceManagerTags` on provisioned instances. The controller receives `roles/resourcemanager.tagUser` only on that key, in addition to the canonical role's instance-side tag-binding permission.
- **`gmp-critical` PriorityClass:** the chart defaults `controller.priorityClassName` to `gmp-critical`, which only the GMP operator creates. `make e2e-deploy` creates it when the cluster has no GMP, otherwise the controller pod is rejected; an existing, GMP-managed class is never modified.

After provisioning, use a dedicated kubeconfig for this cluster. From the repository root, set `E2E_PROJECT_ID=planetscale-karp-gcp-e2e`, `E2E_REGION=asia-southeast1`, `E2E_LOCATION=asia-southeast1` and `E2E_PREFIX=karpenter-e2e`. Select Terraform's dedicated node account for both local-image and release deployments:

```sh
export E2E_NODE_SERVICE_ACCOUNT="$(terraform -chdir=deploy/terraform-e2e output -raw node_service_account)"
make e2e-deploy
```

Then run `make e2e-tests E2E_SELECTION=standard GINKGO_PROCS=4` after approval. Make derives the pod-range and tag-key names from the prefix. Local deploy/cleanup retain their Bash mutation bodies under one canonical Lease held by Go; shared read-only ownership inventory and cloud disappearance proof precede controller replacement. Cleanup preserves the owned test namespace/bootstrap RBAC. The unchanged Make cleanup recipe omits `E2E_REGION`, so local cleanup derives it from `E2E_LOCATION`; non-US deploys must still set `E2E_REGION` explicitly. Prepared CI never derives missing target configuration.

`standard` includes basic Local SSD tests but excludes GPU and extended Local SSD tests. Check available quota and capacity before increasing parallelism. Helm releases and per-run test resources are not managed by Terraform. `make e2e-setup` and `make e2e-teardown` are retired.

## Retirement

After destructive approval:

1. Stop test runs and remove test workloads so they cannot trigger new nodes.
2. Keep Karpenter running while removing test NodePools/NodeClaims. Wait for their VMs to disappear and check for orphaned VMs, disks and other per-run resources.
3. Only then remove the controller and retire Terraform-owned infrastructure. Keep the shared state bucket available until retirement completes.

## Important limits

- The e2e stack disables deletion protection and defines no `prevent_destroy` guards, so retirement needs no lifecycle-guard edit. Nothing prevents an accidental `terraform destroy`; the shared target is protected only by review and state locking. Regional creation can temporarily provision default nodes in multiple zones before Terraform removes that initial pool; the separately managed `default-pool` has one persistent node.
- The registry deletes **all tagged and untagged images older than seven days**. A running controller may fail to restart after its image is deleted; redeploy a fresh image before relying on an older installation.

## Opt-in manual CI identity (not activated)

`ci_enabled` defaults to `false`. Existing manual targets gain no CI service account, federation or runtime grants. After a separately approved plan/apply, opting in creates `${prefix}-ci-sa`, a dedicated `${prefix}-ci` WIF pool and `github` provider. It does not configure GitHub, bootstrap Kubernetes access, enable any preset or authorize a run. No legacy WIF stack, GitHub provider or Kubernetes Terraform provider is used.

Set `ci_pod_range_names = { small = "<reviewed-existing-key>", large = "<reviewed-existing-key>" }` explicitly when enabling CI. Both keys must exist in `additional_pod_ranges`, be distinct IPv4 ranges, and match the suites' semantics: small `/22` or richer, large strictly richer than small. No map-order inference or CI range creation is allowed. Actual attachment, subnet membership and available capacity still require runtime preflight and operational validation.

Outputs for reviewed configuration (never dump state/private inputs into Actions):

- `ci_workload_identity_provider`, `ci_runtime_service_account`: auth action provider and impersonated service account; null when off.
- `ci_oidc_audience`: pass explicitly as the auth action's `audience`, not its default. Only this exact `https://iam.googleapis.com/projects/<number>/locations/global/workloadIdentityPools/<pool>/providers/github` audience is accepted.
- `ci_environment_variables`: fixed `E2E_*` target values consumed by the trusted coordinator, including explicit `E2E_PODS_RANGE_NAME`, `E2E_SMALL_PODS_RANGE_NAME`, `E2E_LARGE_PODS_RANGE_NAME` and `E2E_RESOURCE_MANAGER_TAG_KEY`. It is configuration, not an enable flag. `additional_pod_ranges` exports the existing complete map; `resource_manager_tag_key` exports `id`, `name` and `short_name`. Tests require the **short name**, not `tagKeys/<id>`, and construct `project/short_name` themselves.

### GitHub operator configuration (separately approved)

Repository variables are administrative settings, not dispatch inputs:

| Variable                      | Value                                                                                     |
|-------------------------------|-------------------------------------------------------------------------------------------|
| `E2E_CI_ENABLED`              | Literal `true` only after activation approval; unset/false disables.                      |
| `E2E_ENABLED_PRESETS`         | Comma-separated validated subset of `standard,gpu,full`; empty disables all.              |
| `E2E_TARGET_CONFIG`           | JSON object from `ci_environment_variables`, exactly its twelve explicit `E2E_*` entries. |
| `E2E_WIF_PROVIDER`            | `ci_workload_identity_provider` output.                                                   |
| `E2E_WIF_AUDIENCE`            | `ci_oidc_audience` output (explicit HTTPS audience).                                      |
| `E2E_RUNTIME_SERVICE_ACCOUNT` | `ci_runtime_service_account` output.                                                      |

Never copy private inputs/state into GitHub. Configure environment **`e2e-runtime`** for upstream `main` only, with no required reviewers or unrelated secrets; authorized dispatch is the run/cost approval. Missing environment restrictions block activation. The gate freezes source and target configuration; pre-authentication recheck rejects changed configuration, disabled presets or revoked actor permission. Disable CI to reject queued runs; stop active workers separately. A variable edit does not cancel active execution.

See [manual workflow and public evidence](../../hack/e2e-runner/README.md#manual-pr-workflow) for dispatch, artifact handling and cancellation recovery. Terraform creates neither these settings nor Kubernetes bootstrap, and defaults remain off.

### Exact upstream trust

Issuer is `https://token.actions.githubusercontent.com`. The provider requires every conjunct in `ci-oidc-condition.cel`:

| Claim                 | Exact value                                                                       |
|-----------------------|-----------------------------------------------------------------------------------|
| `repository_id`       | `863038341`                                                                       |
| `repository_owner_id` | `166281999`                                                                       |
| `ref`                 | `refs/heads/main`                                                                 |
| `workflow_ref`        | `cloudpilot-ai/karpenter-provider-gcp/.github/workflows/e2e.yaml@refs/heads/main` |
| `event_name`          | `workflow_dispatch`                                                               |
| `environment`         | `e2e-runtime`                                                                     |
| `sub`                 | `repo:cloudpilot-ai/karpenter-provider-gcp:environment:e2e-runtime`               |

Only `google.subject` and these attributes are mapped. The runtime account grants `roles/iam.workloadIdentityUser` only to `attribute.repository_id/863038341` in this dedicated pool; the provider enforces the full conjunction and exact audience. No wildcard/fork/rehearsal identity, other workflow/ref/event or `job_workflow_ref` (reusable workflow) dependency is admitted. Checking out an authorized fork PR does not change the upstream workflow token's identity. A future reusable workflow or claim change requires explicit trust review, not a permissive fallback.

### Runtime call/authority inventory

`ci-runtime-role.yaml` is separate from the controller's canonical `deploy/iam/karpenter-controller-role.yaml`. All grants are additive IAM members. The runtime is **not** assigned the controller role.

| Actual caller / operation                                                                                 | Authority                                                                                                                  |
|-----------------------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------|
| Coordinator `gcloud container clusters describe`, get-credentials; tests `Clusters.Get`, `NodePools.List` | Target-project `container.clusters.get`; Kubernetes API authorization is separate                                          |
| Channel-image tests `GetServerConfig`                                                                     | Target-project `container.getServerConfig`                                                                                 |
| Coordinator instance/disk inventory; tests VM/disk inspection and deletion confirmation                   | Target-project `compute.instances.get/list`, `compute.disks.get/list`; no cloud delete/create/update                       |
| Coordinator subnet describe; tests `Subnetworks.Get` with `WITH_UTILIZATION`                              | Target-project `compute.subnetworks.get`, including existing primary/additional pod ranges                                 |
| Networking tests regional Resource Manager `effectiveTags.list` on a Compute instance                     | Target-project `compute.instances.listEffectiveTags`; no tag binding writes                                                |
| Coordinator namespaced tag-key describe and values list                                                   | `roles/resourcemanager.tagViewer` on only the Terraform fixture key                                                        |
| Prepared OCI upload/digest reads through crane + gcloud credential helper                                 | `roles/artifactregistry.writer` on only the Terraform image repository                                                     |
| Tests `Images.List` in `gke-node-images` and `ubuntu-os-gke-cloud`                                        | Existing Google-managed public image-project access for authenticated principals, **not** the target project's custom role |

The cloud calls above are in `hack/e2e-runner/{target,drain,prepared}.go` and `test/pkg/environment/{environment,helpers,podrange,resourcemanagertags}.go`. GPU and extended SSD tests assert through Kubernetes/these same read clients; they do not require additional direct runtime cloud mutation permissions. Image listing in public projects must be proven operationally; if denied, stop for review, not project-wide image/admin grants. Terraform provisioning needs its own administrative identity (including project-number discovery), never this runtime identity.

There is no direct runtime SA `actAs`, token-creator, IAM administration, custom-role administration, backend/state or maintenance grant. Controller node-SA `actAs` stays scoped to the existing dedicated node account; controller tag use stays on the fixture key. Do not duplicate controller permissions in this role to make tests pass.

### Reviewed Kubernetes bootstrap (manual example only)

`ci-bootstrap-rbac.yaml` is an explicit **review-before-use example**, never applied by Terraform, workflow or runner. Replace `CI_RUNTIME_SERVICE_ACCOUNT_EMAIL` with only `ci_runtime_service_account`. An owner-approved administrator must separately inspect existing objects/ownership and provision missing bootstrap; do not blindly apply this file over an existing GMP PriorityClass, active Lease, namespaces or RBAC. Runtime must not repair or reset them. No Kubernetes Terraform provider is added.

The example precreates `karpenter-system`, the canonical `karpenter-e2e-tests` Lease, a non-preempting `gmp-critical` PriorityClass for targets without GMP, and the persistent `karpenter-e2e-test` namespace with `karpenter-e2e/owned=true`. Its exact `e2e-runtime-test-workloads` RoleBinding names the same ClusterRole and dedicated runtime User. Preflight verifies existing namespace identity/label and binding role/subject before pushing images. Both survive cleanup unchanged. Missing, unlabeled, mismatched or terminating bootstrap fails closed; no adoption/relabeling.

RBAC is source-inventoried from the current chart Roles/ClusterRoles, coordinator and tests. **No cluster-admin, wildcard API/resource grants, bind, escalate or impersonate**. To install the current chart without bypassing Kubernetes anti-escalation, runtime holds the chart's exact existing permissions (including current global pod reads/deletion/eviction, node mutation and CSR/kubelet-signer approval). This is substantial existing authority, not a minimal read-only Kubernetes identity. Current Helm role objects can be created and only current role names updated; a chart adding permissions runtime lacks must fail anti-escalation and need separate reviewed maintenance, not a privilege-bypass grant. CRD creation cannot be name-constrained; updates are restricted to the five current Karpenter CRDs. Authenticated discovery uses the cluster's existing standard discovery bindings.

Deployment writes are scoped to the controller namespace (current Helm objects) and preserved test namespace, **not** all namespaces. Helm Secrets stay in `karpenter-system`; none in unrelated namespaces are directly granted. Test namespace initialization GETs first; CI has no Namespace create/delete/patch grant. Local NotFound-only creation remains supported. Chart Role lease-creation authority is retained where the existing controller needs it, but normal coordinator code only uses the preexisting canonical Lease.

Under the Lease, cleanup inventories the namespace's Deployments, ReplicaSets and Pods. Only label-owned Deployment roots plus UID-matched controller descendants can be drained; foreground deletion is bounded and label/name-filtered. Namespace UID and bootstrap binding remain intact. Current test helpers create some **unlabeled Deployments**; normal test cleanup is responsible, and any such leftovers block for maintenance. Namespace/name/Pod selectors never imply ownership. This intentionally fails closed instead of adopting legacy resources or deleting the namespace to hide incomplete cleanup. The old namespace-deletion compatibility contract is superseded by the owner's persistent-bootstrap choice.

### Activation and transitive authority audit

This is **privileged execution of maintainer-authorized arbitrary PR controller, charts and test binaries in a dedicated project**, not sandbox isolation. Frozen SHAs and hashes establish attribution/transfer integrity, not safety. Chart RBAC, CRDs and controller workloads can exercise the controller's GKE Workload Identity; that identity can mutate GCE/GKE using its canonical role and attach the dedicated node SA. Test workloads/allowed node operations may reach node or other in-cluster identities. Public artifacts/redaction cannot contain deliberately malicious code.

Before activation, separately review the exact plan plus effective inherited IAM, SA policies/keys, workload-identity bindings, node scopes/metadata access, all reachable KSAs/Secrets/workloads, network paths and project/org grants for **runtime, controller, node and workload identities**. The same project contains the separately managed state bucket: confirm no reachable identity can read/write it, impersonate provisioning or maintenance users, administer IAM or reach unrelated systems. Also audit allowed role-object creation/updates and the complete current controller Kubernetes permission superset, not just direct cloud permissions; no bind/escalate bypass is granted. Any such reachability blocks activation until explicitly resolved. Do not run a live permission probe as part of offline validation.

Operational gates remain separate:

1. Approve and review exact Terraform changes before any apply. APIs (IAM, STS, IAM Credentials, Artifact Registry, GKE, Compute and Resource Manager) must already be available; runtime never enables services or repairs infrastructure.
2. Manually configure environment `e2e-runtime` for upstream `main` only, **without required reviewers**. Dispatch itself is approval. Keep the workflow administrative enable flag off and enabled-presets list empty until separately authorized. Copy only reviewed non-secret outputs into fixed target configuration; no dispatch target/ref/shell/worker overrides. Global Ginkgo cap is the owner-selected **20**.
3. Prove cloud-free frozen fork-head check publication/artifact handoff first. Prove positive upstream-main dispatch and negative fork/wrong-ref/workflow/event token exchanges without broadening WIF. Candidate staging and post-merge upstream proof are distinct approvals.
4. Use refreshable URL-backed `external_account` credentials via the pinned auth action. Keep credentials/kubeconfig outside artifacts and results. Prove refresh beyond both initial GitHub OIDC and impersonated-token expiry for **gcloud, docker-credential-gcloud/crane, GKE client-go plugin and Go ADC** over the actual run duration. No keys, fixed access-token environment or token-creator workaround.
5. Separately enable/validate `standard`, `gpu` and `full`, including capacity/cost, ownership-safe cleanup, cancellation and local/CI contention. `full` is exact local `all`, including GPU and extended SSD. No quota waves, downgrade or automatic enable.
6. Maintenance requires CI disabled, all workers/local mutators stopped and separately approved exclusion/investigation, including during Kubernetes API outages. State locking does not own the e2e Lease. Normal CI must fail closed on absent bootstrap, abandoned holders, uncertain ownership or the sticky maintenance marker.

### Cloud-free identity checks

From the repository root, `go test ./deploy/terraform-e2e` evaluates the shipped CEL allow/deny fixtures and the source-inventoried permission manifest. This is not token exchange/permission-probe evidence. Terraform mocks check default-off behavior, scoped grants, exact issuer/audience/mapping and range/output validation.

To avoid loading any local private tfvars/overrides or backend metadata, validate only allowlisted configuration in a fresh project-local scratch directory:

```sh
mkdir -p .pi
scratch=$(mktemp -d .pi/terraform-ci.XXXXXX)
mkdir -p "$scratch/deploy/terraform-e2e/tests" "$scratch/deploy/terraform" "$scratch/deploy/iam"
cp deploy/terraform-e2e/{main,network,tags,versions,variables,outputs,ci-identity}.tf \
  deploy/terraform-e2e/{.terraform.lock.hcl,ci-runtime-role.yaml,ci-oidc-condition.cel} \
  "$scratch/deploy/terraform-e2e/"
cp deploy/terraform-e2e/tests/ci-identity.tftest.hcl "$scratch/deploy/terraform-e2e/tests/"
cp deploy/terraform/{main,iam,variables,system_node_pool}.tf "$scratch/deploy/terraform/"
cp deploy/iam/karpenter-controller-role.yaml "$scratch/deploy/iam/"
terraform -chdir="$scratch/deploy/terraform-e2e" init -backend=false -input=false -lockfile=readonly
terraform -chdir="$scratch/deploy/terraform-e2e" validate
terraform -chdir="$scratch/deploy/terraform-e2e" test -filter=tests/ci-identity.tftest.hcl
```

All test runs specify `command = plan` with a mocked Google provider, not live plans or applies. The provider version stays pinned; no upgrades. `go test ./deploy/terraform-e2e` also audits the bootstrap and, when installed, uses real Helm to render the current RBAC templates/values without unrelated subchart dependencies; it checks the current roles are covered without bind/escalate and unrelated permission expansion is denied. If Helm is absent that source-render audit is explicitly skipped, never installed implicitly. These checks do not prove real CEL/token exchange, IAM permission support, Kubernetes admission/RBAC or long-lived auth refresh. Those remain separately approved activation checks.

Offline pipeline/security regressions are not wired into the unchanged upstream `make ci`/presubmit. From the repository root, run them explicitly:

```sh
node --test .github/scripts/e2e/*_test.cjs
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s .github/scripts/e2e -p '*_test.py'
go test -race ./hack/e2e-runner ./deploy/terraform-e2e ./test/pkg/environment -count=1 -timeout=300s
```

# Persistent e2e environment

This Terraform root defines a **new** persistent GKE e2e target for manual runs.
It uses `../terraform` for the VPC, subnet, cluster, controller/node accounts,
controller role and persistent system node pool. This root configures one
30-GB system node of the selected machine type and adds NAT, the image registry
and Resource Manager tag fixtures.
The module reads controller permissions from
`deploy/iam/karpenter-controller-role.yaml`. Existing local infrastructure is
not imported or retired automatically. `make e2e-setup` and `make e2e-teardown`
are retired; they print this path rather than touching GCP.

The image repository deletes **tagged and untagged** versions older than seven
days, with no keep exception. If a controller image is deleted while its pod
still runs, a later restart may fail to pull that digest; redeploy an image
before relying on an older installation. Review the policy in the approved
Terraform plan before applying; cleanup is asynchronous, not an exact-time TTL.

The project and billing account must already exist. Confirm owner and permitted
operations, enabled APIs, Singapore zone and machine availability (including
GPU/ARM where needed), quota, budget, CIDR non-overlap and resource-name
collisions before provisioning. This root does not enable APIs or manage a
GitHub identity. The cluster has deletion protection and the system pool has
`prevent_destroy`; retirement requires a separate reviewed code change and
explicit destructive approval. The module protects the system pool for all
callers, independently of cluster deletion protection.

## Release channel and pod ranges

`release_channel` defaults to `"STABLE"` in both this root and the reusable
installation module. Select `"REGULAR"`, `"RAPID"` or `"EXTENDED"` when needed;
there is no explicit Kubernetes version input. GKE selects the initial version
and manages auto-upgrades within the channel. The system pool inherits the
master version on creation. Verify actual control-plane and node versions
before testing. Channel enrollment or changes can trigger upgrades, not
downgrades, and require a maintenance window and explicit apply consent.

`additional_pod_ranges` maps subnet secondary range names to CIDRs and attaches
those ranges to GKE. The shared configuration includes a small `/22` and a large `/20` for
the multi-range networking tests. Review all ranges for overlap before applying.
An empty map preserves a single pod range, but multi-range tests require both
additional ranges. With the committed names, pass `E2E_PREFIX=tf-e2e-sgp` to
Make; it derives `E2E_PODS_RANGE`, `E2E_SMALL_PODS_RANGE` and
`E2E_LARGE_PODS_RANGE`. Override the latter two when using different map keys.
Changing `prefix` also requires updating the map keys. Both candidate
ranges need at least 1,024 free IPs for the ranking test.

The project-scoped `${prefix}-tag` key and its `e2e` value support the
`resourceManagerTags` tests. The controller receives `roles/resourcemanager.tagUser`
only on that key. This is separate from the instance-side tag-binding permission
already supplied by the canonical controller role; the fixtures and their
key-scoped grant are e2e-specific. Make derives `E2E_RESOURCE_MANAGER_TAG_KEY` from `E2E_PREFIX`
and passes it to both test targets; use the Terraform prefix for the runner.

## State and validation

`terraform.tfvars` contains the shared non-secret target configuration for
`planetscale-karp-gcp-e2e`. The GCS backend uses bucket
`planetscale-karp-gcp-e2e-tfstate` in that project and prefix `karpenter/e2e`,
so maintainers use the same state. The bucket must exist before backend
initialization; it is not managed by this stack. Use object versioning,
uniform bucket-level access and public-access prevention. The GCS backend
provides state locking; do not disable it.

Authenticate through Application Default Credentials, not committed keys.
Keep credentials, state, saved plans and local overrides out of Git; the
committed tfvars must contain only non-secret configuration. If resources
already exist at the selected target, agree imports individually before
applying; never allow an unreviewed replacement.

Offline checks from this directory:

```sh
terraform fmt -check -recursive
terraform init -backend=false -input=false -lockfile=readonly
terraform validate
terraform -chdir=../terraform init -backend=false -input=false
terraform -chdir=../terraform validate
```

For approved remote-state access, initialize the shared backend:

```sh
terraform init -input=false -lockfile=readonly
```

Do not override the backend bucket/prefix or use a separate workspace for this
environment. Existing local state requires a separately reviewed migration;
do not discard it or copy it manually. Inspect a private refresh/plan for
resource additions, IAM breadth, replacements or deletes.
Get **separate consent** for backend creation, each import/apply/destroy,
GitHub settings and live e2e. A repeat plan must have no unexplained drift.
Neither merging this code nor the offline checks authorizes a live operation.

Terraform backend locking does not exclude test/deploy clients. Before
maintenance, agree a window, block new local/CI admission and verify active
jobs have stopped. The runner's Lease covers only `make e2e-tests`; it cannot
protect initial creation or an API outage. If ownership cannot be checked,
stop rather than assuming the target is idle. Use a distinct kubeconfig for
the sponsored cluster. After provisioning, deploy the controller from the
checkout with `make e2e-deploy` and explicit `E2E_PROJECT_ID`, `E2E_REGION`,
`E2E_LOCATION`, `E2E_PREFIX` and kubeconfig. Run tests only after separately
approved deployment and source-alignment checks. Per-run Helm releases, tests,
Karpenter-created nodes and GKE-managed firewall rules are not Terraform
resources. CI runtime identity, WIF, protected workflow and token exchange
belong to Phase 3; Phase 2 does not grant CI access.

## Standard + Local SSD capacity after #628

The runner's `standard` selection (`!suite:gpu`) includes #628's
`suite:local-ssd` specs; do not start a second Local SSD runner. Start with
`GINKGO_PROCS=4` and measure actual peak nodes, CPU/Local SSD usage and
cleanup overlap. Only try 6 for speed after that evidence and cost/quota
approval. A mixed-pool spec holds three nodes (8- and 4-vCPU N2D plus 2-vCPU
N2). Using a deliberately conservative envelope of 14 vCPU and eight 375-GB
N2D SSD partitions per worker, 25% reserve, and an example 2-vCPU system node:

| Workers | All-family CPU | N2D CPU | N2D Local SSD |
|---:|---:|---:|---:|
| 4 | 73 vCPU | 60 vCPU | 15,000 GB |
| 6 | 108 vCPU | 90 vCPU | 22,500 GB |
| 8 (stretch) | 143 vCPU | 120 vCPU | 30,000 GB |

These are **planning envelopes**, not measured quota requests. Recalculate
for the actual system node, usage, other families, regional/all-regions CPU,
per-family regional/zonal Local SSD, Hyperdisk/PD, addresses and zonal stock.
Request only the gap between needed headroom plus current usage and the actual
quota limit. Do not include capacity-constrained z3 or GPU in the standard
baseline. References: [Compute allocation quotas](https://docs.cloud.google.com/compute/resource-usage)
and [Local SSD quota by family](https://cloud.google.com/compute/docs/quotas/migrate-local-ssd-quota).

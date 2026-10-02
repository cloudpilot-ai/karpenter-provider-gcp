# Persistent e2e environment

This Terraform root defines a **new** persistent GKE e2e target for manual runs.
It uses `../terraform` for the VPC, subnet, cluster, controller/node accounts
and controller role; this root adds the system node pool, NAT and image registry.
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
explicit destructive approval.

## State and validation

A maintainer must bootstrap a separate, access-controlled GCS backend outside
this stack, with versioning, retention/recovery, encryption and locking. Choose
bucket/prefix and maintenance identity before first init. Keep real tfvars,
backend config, state and saved plans private and untracked. Never grant test
credentials access to the state or maintenance identity. If resources already
exist at the selected target, agree imports individually before applying;
never allow an unreviewed replacement.

Offline checks from this directory:

```sh
terraform fmt -check -recursive
terraform init -backend=false -input=false -lockfile=readonly
terraform validate
```

`terraform.tfvars.example` is a placeholder, not a deployable target. For an
approved target only, initialize the private backend and inspect a private
refresh/plan for resource additions, IAM breadth, replacements or deletes.
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

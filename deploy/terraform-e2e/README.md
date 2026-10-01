# Persistent e2e environment (offline configuration)

This root defines a **new, dedicated** GKE e2e environment. It does not import
or manage an existing local test cluster. It is not part of normal `e2e-run`;
only an approved maintenance operation may run Terraform. CI and test code must
not have state-bucket or infrastructure-maintenance access.

## Ownership

Terraform owns one protected zonal cluster and system node pool, VPC/subnet and
ranges, router/NAT, image registry, controller/node/runtime service accounts,
controller custom role and narrowly scoped bindings. The role reads permissions
from `../iam/karpenter-controller-role.yaml`. Helm releases, e2e test resources,
Karpenter-created nodes and GKE-managed network rules are **not** Terraform
resources. The `tf-e2e-` prefix is reserved; legacy `e2e-setup` and
`e2e-teardown` reject it before contacting GCP. Do not run them with an alternate
prefix against Terraform resources.

The project and billing account must exist. Confirm required service APIs,
quota, budget, location, machine and disk availability, CIDR non-overlap and
resource-name collisions before any real plan. This root does not enable project
APIs or manage GitHub settings. If the sponsored project already contains named
resources, agree an import and ownership handoff before apply; never allow an
unreviewed replacement. The system pool and cluster have deletion protection.
Retirement requires a separate maintenance plan and approval.

## State and review gates

A maintainer owns a separate GCS backend, created outside this stack with
versioning, access controls, retention/recovery and locking. Choose its bucket,
prefix, access principal and security settings before first initialization;
store backend config and real variable values outside Git. State and saved plans
contain sensitive values. No runtime/PR identity may access the bucket.

Offline checks from this directory:

```sh
terraform fmt -check -recursive
terraform init -backend=false -input=false -lockfile=readonly
terraform validate
terraform test
```

`terraform.tfvars.example` shows input *shapes*, not an approved target.
Neither running the checks nor merging this configuration authorizes backend
creation, import, apply, destroy, a GitHub settings change or an e2e run.
After explicit approval for a named target and operation, a maintainer should
initialize using private backend config, inspect a saved plan privately for
unexpected additions/replacements/deletions and IAM breadth, then seek separate
approval to apply. Confirm a repeat plan has no unexplained drift. Never upload
state or raw plans as PR artifacts.

Before maintenance, exclude all local/CI/test/deploy clients, verify active jobs
have stopped and obtain an approved window. The runner's Kubernetes Lease covers
`make e2e-tests` only; it cannot protect initial creation or API outages.
Terraform's GCS lock protects state, **not** concurrent GKE clients. If the
cluster or Lease cannot be checked, stop rather than assuming no one is running.
Use a distinct kubeconfig for this environment; do not overwrite a maintainer's
existing local e2e kubeconfig.

## CI trust and activation

`enable_ci_wif` defaults to false. The reserved identity contract is upstream
`cloudpilot-ai/karpenter-provider-gcp`, default-branch
`.github/workflows/e2e-manual.yaml`, protected GitHub environment `e2e` and
`workflow_dispatch`. Numeric repository/owner IDs are supplied explicitly and
must be reverified. The provider condition restricts repository, workflow ref,
branch, event and environment subject; the runtime account cannot update IAM or
read Terraform state. **Do not enable WIF before** the protected environment,
actual OIDC claims and workflow trust model have been reviewed and approved.
The manual workflow, runtime Kubernetes RBAC, token refresh and real exchange
are later-stage deliverables; Terraform validation is not proof of access.

Before CI activation, verify the runtime identity can perform intended read,
registry and Kubernetes operations and cannot administer IAM, destroy the
cluster, impersonate controller/node/maintenance identities or read backend
state. The controller may attach only the dedicated node account. A local run
against this environment requires separate approval and an explicit, distinct
kubeconfig. GPU, ARM and high-parallelism Local SSD testing require independent
capacity/quota checks and cost approval.

## Standard + Local SSD capacity after #628

The merged runner's `standard` selection (`!suite:gpu`) includes #628's
`suite:local-ssd` specs; it is **not** a second suite to run concurrently.
Start with the existing `GINKGO_PROCS=4` and record actual concurrent nodes,
family-specific CPU/Local SSD usage, duration and cleanup overlap. Only try
`GINKGO_PROCS=6` for speed after that evidence and quota/cost review; do not
assume 6 is faster than 4. The #628 mixed-pool spec holds three nodes
(8-vCPU and 4-vCPU N2D plus 2-vCPU N2) concurrently. For a conservative
preflight bound of 14 vCPU and eight 375-GB N2D SSD partitions per worker,
plus 25% reserve and an example 2-vCPU system node:

| Workers | All-family CPU envelope | N2D CPU envelope | N2D Local SSD envelope |
|---:|---:|---:|---:|
| 4 | 73 vCPU | 60 vCPU | 15,000 GB |
| 6 | 108 vCPU | 90 vCPU | 22,500 GB |
| 8 (stretch only) | 143 vCPU | 120 vCPU | 30,000 GB |

These are planning envelopes, **not measured requirements or quota-increase
requests**. Check actual regional and all-regions CPU, per-family regional/zonal
Local SSD quota (`LOCAL_SSD_TOTAL_GB_PER_VM_FAMILY`), C4D/C4A and other
family-specific SSD/CPU limits, Hyperdisk/PD, addresses, instances, API rates
and zonal stock. Recalculate for the approved system node and existing usage;
request only `max(0, envelope + existing usage - current limit)` for each
metric. Do not turn on capacity-constrained z3 (`E2E_Z3_TESTS`) or GPU as part
of the standard baseline. See [GCE quota guidance](https://docs.cloud.google.com/compute/resource-usage)
and [Local SSD per-family quota guidance](https://cloud.google.com/compute/docs/quotas/migrate-local-ssd-quota).

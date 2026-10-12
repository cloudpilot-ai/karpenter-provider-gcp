# GKE infrastructure for Karpenter

This Terraform root can also be used as a module. It creates a VPC, subnet,
GKE cluster, controller IAM role and service account, and a persistent system
node pool. Install Karpenter separately using the controller identity output;
the system pool provides capacity for Karpenter and essential cluster workloads
before Karpenter provisions application nodes.

The system pool uses the node service account and Workload Identity metadata.
For an existing node service account, its IAM permissions remain caller-managed.
The controller role reads `../iam/karpenter-controller-role.yaml`.

## System pool

| Input | Default | Meaning |
|---|---|---|
| `system_machine_type` | `n2-standard-2` | System-node machine type |
| `system_node_count` | `1` | Nodes per zone, not the total for a regional cluster |
| `system_disk_size_gb` | `30` | Boot disk size per node |

The pool is named `${common_name}-system` and is not managed by Karpenter.
Regional clusters can create nodes in multiple zones; account for that
multiplier when sizing capacity and cost.

The system pool has `prevent_destroy = true` for every caller, independently
of the cluster's `deletion_protection` setting. Destruction or a change requiring
pool replacement requires an explicit, reviewed edit to that lifecycle guard.
Terraform lifecycle settings cannot be parameterized. This protection does not
prevent deletion outside Terraform.

## Resource Manager tags

The controller role includes the instance-side permission to create tag
bindings. Using a Resource Manager tag also requires permission on the chosen
tag key or value. Tag owners must grant the controller appropriately scoped
access separately; this module does not create tag keys or grant project-wide
tag-user access. The e2e root owns its test tag fixtures.

## Offline validation

```sh
terraform fmt -check
terraform init -backend=false -input=false
terraform validate
```

Infrastructure creation, changes and deletion require separate approval.

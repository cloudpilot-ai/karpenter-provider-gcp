# GKE infrastructure for Karpenter

Use this directory as a Terraform root or module. It creates a VPC, subnet, GKE cluster, controller/node identities and a persistent system node pool. Install Karpenter separately using the controller identity output; the system pool provides capacity for Karpenter and essential cluster workloads.

Controller permissions come from `../iam/karpenter-controller-role.yaml`. The system pool uses the node service account and Workload Identity metadata. Permissions for an existing node service account remain caller-managed.

## System pool

| Input                 | Default         | Meaning                                |
|-----------------------|-----------------|----------------------------------------|
| `system_machine_type` | `n2-standard-2` | Machine type                           |
| `system_node_count`   | `1`             | Nodes per zone, not the regional total |
| `system_disk_size_gb` | `30`            | Boot disk size per node                |

The pool is named `${common_name}-system` and is not managed by Karpenter. Account for multiple zones when sizing regional clusters.

`prevent_destroy = true` protects the pool for every caller, independently of cluster deletion protection. Destroying or replacing it requires explicitly editing that lifecycle guard. It does not prevent deletion outside Terraform.

## Resource Manager tags

The controller role grants instance-side tag-binding permission. To use `spec.resourceManagerTags`, separately grant access to the chosen tag keys or values. This module creates no tags or tag-user grants; test fixtures belong to the [e2e root](../terraform-e2e/README.md).

## Offline validation

```sh
terraform fmt -check
terraform init -backend=false -input=false
terraform validate
```

Review a plan and obtain approval before applying infrastructure changes.

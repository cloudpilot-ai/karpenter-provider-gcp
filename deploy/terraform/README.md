# GKE infrastructure for Karpenter

Use this directory as a Terraform root or module. It creates a VPC, subnet, GKE cluster, controller/node identities and a persistent system node pool. Install Karpenter separately using the controller identity output; the system pool provides capacity for Karpenter and essential cluster workloads.

Controller permissions come from `../iam/karpenter-controller-role.yaml`. The system pool uses the node service account and Workload Identity metadata. Permissions for an existing node service account remain caller-managed.

## System pool

| Input                 | Default         | Meaning                                |
|-----------------------|-----------------|----------------------------------------|
| `system_machine_type` | `n2-standard-2` | Machine type                           |
| `system_node_count`   | `1`             | Nodes per zone, not the regional total |
| `system_disk_size_gb` | `30`            | Boot disk size per node                |
| `system_node_pool_name` | `${common_name}-system` | Pool name                     |
| `system_node_locations` | Cluster defaults | Pool zones; pin one for one total node |

The pool is not managed by Karpenter. Regional clusters multiply the node count by the pool's zones. Set `system_node_locations` independently of the cluster's `node_locations` to avoid multiplying persistent capacity.

`cluster_location` accepts a zone or region. `node_locations` selects default worker zones; omitted values retain GKE defaults. `controller_role_id` overrides the custom-role ID, which otherwise defaults to `${common_name}` with hyphens replaced by underscores, suffixed with `_controller`.

Cluster deletion protection is configurable; the system pool has no hard-coded destruction guard.

## Observability

`cloud_observability_enabled` (default `false`) controls GKE-managed Cloud Logging and Cloud Monitoring components and managed Prometheus. When disabled the cluster reports no logs or metrics and the managed Prometheus operator is not deployed; enable it, or run your own stack, when the deployment needs cluster telemetry. GKE addons keep their platform defaults.

## Resource Manager tags

The controller role grants instance-side tag-binding permission. To use `spec.resourceManagerTags`, separately grant access to the chosen tag keys or values. This module creates no tags or tag-user grants; test fixtures belong to the [e2e root](../terraform-e2e/README.md).

## Offline validation

```sh
terraform fmt -check
terraform init -backend=false -input=false
terraform validate
```

Review a plan and obtain approval before applying infrastructure changes.

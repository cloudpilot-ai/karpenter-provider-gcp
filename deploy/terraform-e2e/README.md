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

- **Pod ranges:** the committed small `/21` and large `/19` ranges are attached to GKE for multi-range networking tests. Each candidate needs at least 1,024 free IPs for the ranking test; both are at least twice that, so a few node allocations cannot drop a candidate below the threshold. This adds headroom but does not remove the controller's free-IP cache and status refresh latency. Changing the prefix requires updating range names in `terraform.tfvars`.
- **Resource Manager tags:** `${prefix}-tag` and its `e2e` value let tests verify `spec.resourceManagerTags` on provisioned instances. The controller receives `roles/resourcemanager.tagUser` only on that key, in addition to the canonical role's instance-side tag-binding permission.
- **`gmp-critical` PriorityClass:** the chart defaults `controller.priorityClassName` to `gmp-critical`, which only the GMP operator creates. `make e2e-deploy` creates it when the cluster has no GMP, otherwise the controller pod is rejected; an existing, GMP-managed class is never modified.

After provisioning, use a dedicated kubeconfig for this cluster. From the repository root, set `E2E_PROJECT_ID=planetscale-karp-gcp-e2e`, `E2E_REGION=asia-southeast1`, `E2E_LOCATION=asia-southeast1` and `E2E_PREFIX=karpenter-e2e`. Select Terraform's dedicated node account for both local-image and release deployments:

```sh
export E2E_NODE_SERVICE_ACCOUNT="$(terraform -chdir=deploy/terraform-e2e output -raw node_service_account)"
make e2e-deploy
```

Then run `make e2e-tests E2E_SELECTION=standard GINKGO_PROCS=4` after approval. Make derives the pod-range and tag-key names from the prefix.

`standard` includes basic Local SSD tests but excludes GPU and extended Local SSD tests. Check available quota and capacity before increasing parallelism. Helm releases and per-run test resources are not managed by Terraform. `make e2e-setup` and `make e2e-teardown` are retired.

## Retirement

After destructive approval:

1. Stop test runs and remove test workloads so they cannot trigger new nodes.
2. Keep Karpenter running while removing test NodePools/NodeClaims. Wait for their VMs to disappear and check for orphaned VMs, disks and other per-run resources.
3. Only then remove the controller and retire Terraform-owned infrastructure. Keep the shared state bucket available until retirement completes.

## Important limits

- The e2e stack disables deletion protection and defines no `prevent_destroy` guards, so retirement needs no lifecycle-guard edit. Nothing prevents an accidental `terraform destroy`; the shared target is protected only by review and state locking. Regional creation can temporarily provision default nodes in multiple zones before Terraform removes that initial pool; the separately managed `default-pool` has one persistent node.
- The registry deletes **all tagged and untagged images older than seven days**. A running controller may fail to restart after its image is deleted; redeploy a fresh image before relying on an older installation.

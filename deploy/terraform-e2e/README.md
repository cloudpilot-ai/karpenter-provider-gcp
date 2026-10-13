# Persistent e2e environment

Shared configuration is committed in `terraform.tfvars`:

- Project: `planetscale-karp-gcp-e2e`
- Regional control plane: `asia-southeast1`
- Worker zones: `asia-southeast1-a`, `asia-southeast1-b`, `asia-southeast1-c`
- Prefix: `karpenter-e2e`; resource names match the former setup script
- System pool: `default-pool`, one `n2-standard-2` node in `asia-southeast1-b` with a 30-GB boot disk
- Release channel: `STABLE`; GKE selects and auto-upgrades Kubernetes versions

The [installation module](../terraform/README.md) creates the network, cluster, system pool and controller/node identities. This root adds NAT, an image registry and test tag fixtures. Controller permissions come from `deploy/iam/karpenter-controller-role.yaml`. APIs and the state bucket must already exist. Existing infrastructure is not imported automatically.

## Shared state

The GCS backend uses bucket `planetscale-karp-gcp-e2e-tfstate` in the same project, with prefix `karpenter/e2e`. Use this backend and the default Terraform workspace so maintainers share one state. The bucket is managed separately; use versioning, uniform bucket-level access and public-access prevention. Keep state locking enabled.

Authenticate with Application Default Credentials. Do not commit credentials, state, saved plans or local overrides; committed configuration is non-secret. From this directory, initialize the existing backend:

```sh
terraform init -input=false -lockfile=readonly
```

Review a plan before applying. Bucket creation, imports, state migration, infrastructure changes and live e2e runs require separate approval. Coordinate maintenance with other users: Terraform state locking does not block tests or deployments.

The existing state tracks the zonal, `tf-e2e-sgp`-named environment. These regional/legacy-name settings require a separately reviewed replacement plan, not a routine apply. The existing custom-role state address owns `tf_e2e_sgp_controller`; resolve that ownership before adopting the preserved `karpenter_controller` role. Do not import over the occupied address or delete the role blindly.

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

After provisioning, use a dedicated kubeconfig for this cluster. From the repository root, set `E2E_PROJECT_ID=planetscale-karp-gcp-e2e`, `E2E_REGION=asia-southeast1`, `E2E_LOCATION=asia-southeast1` and `E2E_PREFIX=karpenter-e2e`. Select Terraform's dedicated node account for both local-image and release deployments:

```sh
export E2E_NODE_SERVICE_ACCOUNT="$(terraform -chdir=deploy/terraform-e2e output -raw node_service_account)"
make e2e-deploy
```

Then run `make e2e-tests E2E_SELECTION=standard GINKGO_PROCS=4` after approval. Make derives the pod-range and tag-key names from the prefix. Legacy targets can leave `E2E_NODE_SERVICE_ACCOUNT` unset to retain the chart default.

`standard` includes basic Local SSD tests but excludes GPU and extended Local SSD tests. Check available quota and capacity before increasing parallelism. Helm releases and per-run test resources are not managed by Terraform. `make e2e-setup` and `make e2e-teardown` are retired.

## Retirement

After destructive approval:

1. Stop test runs and remove test workloads so they cannot trigger new nodes.
2. Keep Karpenter running while removing test NodePools/NodeClaims. Wait for their VMs to disappear and check for orphaned VMs, disks and other per-run resources.
3. Only then remove the controller and retire Terraform-owned infrastructure. Keep the shared state bucket available until retirement completes.

## Important limits

- The e2e stack disables deletion protection and defines no `prevent_destroy` guards, so retirement needs no lifecycle-guard edit. Nothing prevents an accidental `terraform destroy`; the shared target is protected only by review and state locking. Regional creation can temporarily provision default nodes in multiple zones before Terraform removes that initial pool; the separately managed `default-pool` has one persistent node.
- The registry deletes **all tagged and untagged images older than seven days**. A running controller may fail to restart after its image is deleted; redeploy a fresh image before relying on an older installation.

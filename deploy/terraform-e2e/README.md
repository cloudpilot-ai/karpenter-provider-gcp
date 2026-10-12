# Persistent e2e environment

Shared configuration is committed in `terraform.tfvars`:

- Project: `planetscale-karp-gcp-e2e`
- Zone: `asia-southeast1-b`
- Prefix: `tf-e2e-sgp`
- System pool: one `n2-standard-2` node with a 30-GB boot disk
- Release channel: `STABLE`; GKE selects and auto-upgrades Kubernetes versions

The [installation module](../terraform/README.md) creates the network, cluster, system pool and controller/node identities. This root adds NAT, an image registry and test tag fixtures. Controller permissions come from `deploy/iam/karpenter-controller-role.yaml`. APIs and the state bucket must already exist. Existing infrastructure is not imported automatically.

## Shared state

The GCS backend uses bucket `planetscale-karp-gcp-e2e-tfstate` in the same project, with prefix `karpenter/e2e`. Use this backend and the default Terraform workspace so maintainers share one state. The bucket is managed separately; use versioning, uniform bucket-level access and public-access prevention. Keep state locking enabled.

Authenticate with Application Default Credentials. Do not commit credentials, state, saved plans or local overrides; committed configuration is non-secret. From this directory, initialize the existing backend:

```sh
terraform init -input=false -lockfile=readonly
```

Review a plan before applying. Bucket creation, imports, state migration, infrastructure changes and live e2e runs require separate approval. Coordinate maintenance with other users: Terraform state locking does not block tests or deployments.

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

- **Pod ranges:** the committed small `/22` and large `/20` ranges are attached to GKE for multi-range networking tests. Each candidate needs at least 1,024 free IPs for the ranking test. Changing the prefix requires updating range names in `terraform.tfvars`.
- **Resource Manager tags:** `${prefix}-tag` and its `e2e` value let tests verify `spec.resourceManagerTags` on provisioned instances. The controller receives `roles/resourcemanager.tagUser` only on that key, in addition to the canonical role's instance-side tag-binding permission.

After provisioning, use a dedicated kubeconfig for this cluster. From the repository root, set `E2E_PROJECT_ID=planetscale-karp-gcp-e2e`, `E2E_REGION=asia-southeast1`, `E2E_LOCATION=asia-southeast1-b` and `E2E_PREFIX=tf-e2e-sgp`. Deploy with `make e2e-deploy`, then run `make e2e-tests E2E_SELECTION=standard GINKGO_PROCS=4` after approval. Make derives the pod-range and tag-key names from the prefix.

`standard` includes basic Local SSD tests but excludes GPU and extended Local SSD tests. Check available quota and capacity before increasing parallelism. Helm releases and per-run test resources are not managed by Terraform. `make e2e-setup` and `make e2e-teardown` are retired.

## Important limits

- The cluster has deletion protection; the system pool has `prevent_destroy`. Pool replacement or retirement requires an explicit lifecycle-guard edit.
- The registry deletes **all tagged and untagged images older than seven days**. A running controller may fail to restart after its image is deleted; redeploy a fresh image before relying on an older installation.

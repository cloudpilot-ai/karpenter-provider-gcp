# E2E tests

This suite provisions a real GKE cluster with Terraform, installs the local Karpenter chart with Helm, runs live provisioning tests, and destroys everything at the end.

## Prerequisites

- `terraform`
- `gcloud`
- `kubectl`
- `helm`
- GCP service account credentials JSON with permission to create GKE, networking, IAM, and service accounts in the target project

## Inputs

The suite accepts only these inputs:

- `E2E_PROJECT_ID` — target GCP project ID
- `E2E_GCP_CREDENTIALS` — absolute path to the service account JSON file
- `E2E_PREFIX` — prefix applied to all Terraform resources and test resources

## Run

```sh
export E2E_PROJECT_ID=<gcp-project-id>
export E2E_GCP_CREDENTIALS=/absolute/path/to/key.json
export E2E_PREFIX=karpenter-e2e

make e2etests
```

The suite performs these steps:

1. `terraform init` and `terraform apply` in `test/e2e/terraform/`
2. `gcloud container clusters get-credentials` for the created cluster
3. `helm upgrade --install` from `charts/karpenter`
4. Run four provisioning cases:
   - `TestOnDemandAMD64`
   - `TestSpotAMD64`
   - `TestOnDemandARM64`
   - `TestSpotARM64`
5. Delete test workloads and rely on Karpenter consolidation to remove the nodes
6. `terraform destroy` during suite teardown

## Notes

- These tests create billable GCP resources.
- The suite uses Workload Identity for the Karpenter controller, so Helm is installed with `credentials.enabled=false`.
- Terraform defaults the cluster region to `us-central1`.

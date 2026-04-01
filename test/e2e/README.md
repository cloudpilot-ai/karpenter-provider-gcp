# E2E tests

This suite provisions a real GKE cluster directly from Go, runs the local Karpenter controller as a subprocess, executes four live provisioning tests, and tears everything back down with cleanup verification at the end.

## What changed

- No Terraform
- No Helm
- Zonal GKE cluster (`${region}-a`) with the default node pool removed during setup
- Karpenter runs from this repo with `go run ./cmd/controller/main.go`
- Cleanup always runs in LIFO order, also on SIGINT/SIGTERM, and finishes by checking that no prefixed resources remain in the GCP project

## Prerequisites

- `gcloud`
- `kubectl`
- `go`
- GCP service account credentials JSON for the e2e runner (see below)

## Creating the e2e runner service account

The service account used by the test suite needs permissions to create and
destroy GKE clusters, networking, IAM bindings, and child service accounts.

```sh
export E2E_PROJECT_ID=<your-gcp-project-id>
export E2E_SA_NAME=karpenter-e2e-runner

# Create the service account
gcloud iam service-accounts create "$E2E_SA_NAME" \
  --project="$E2E_PROJECT_ID" \
  --display-name="Karpenter e2e test runner"

# Grant required roles
for role in \
  roles/compute.admin \
  roles/container.admin \
  roles/iam.serviceAccountAdmin \
  roles/iam.serviceAccountUser \
  roles/resourcemanager.projectIamAdmin \
  roles/serviceusage.serviceUsageAdmin; do
  gcloud projects add-iam-policy-binding "$E2E_PROJECT_ID" \
    --member="serviceAccount:${E2E_SA_NAME}@${E2E_PROJECT_ID}.iam.gserviceaccount.com" \
    --role="$role" \
    --condition=None
done

# Export a key file
gcloud iam service-accounts keys create ./karpenter-e2e-key.json \
  --iam-account="${E2E_SA_NAME}@${E2E_PROJECT_ID}.iam.gserviceaccount.com"

export E2E_SA_PATH=./karpenter-e2e-key.json
```

| Role | Why |
|---|---|
| `roles/compute.admin` | Create/delete VPCs, subnets, and GKE-managed firewall dependencies |
| `roles/container.admin` | Create/delete the GKE cluster and node pools |
| `roles/iam.serviceAccountAdmin` | Create/delete the suite-managed Google service account |
| `roles/iam.serviceAccountUser` | Manage service-account bindings used during the suite |
| `roles/resourcemanager.projectIamAdmin` | Add/remove project IAM policy bindings |
| `roles/serviceusage.serviceUsageAdmin` | Enable required APIs ahead of time if needed |

## Inputs

The suite accepts these environment variables:

- `E2E_PROJECT_ID` — target GCP project ID; optional when the service account JSON includes `project_id`
- `E2E_SA_PATH` — path to the e2e runner service account JSON file; defaults to `karpenter-e2e-key.json` in the repo root
- `E2E_PREFIX` — prefix applied to all created GCP resources and test resources; defaults to `karp-e2e`

## Run

```sh
make e2etests
```

If you keep the runner key at `./karpenter-e2e-key.json` in the repo root and that
JSON contains `project_id`, the default invocation above is enough. Override
`E2E_SA_PATH`, `E2E_PROJECT_ID`, or `E2E_PREFIX` only when needed.

## Suite flow

Setup is implemented in `test/e2e/setup_test.go` and does the following:

1. Creates a dedicated custom-mode VPC and subnet with secondary pod/service ranges using the Go GCP SDK
2. Creates a dedicated `${prefix}-karpenter` Google service account for the controller and a temporary key file for the subprocess
3. Creates a zonal GKE cluster in `${region}-a`
4. Deletes the bootstrap default node pool so the cluster is left without a standing system pool
5. Fetches kubeconfig with `gcloud container clusters get-credentials`
6. Creates the test namespace, `karpenter-system` namespace, and a Kubernetes service account for Workload Identity binding
7. Applies CRDs from `charts/karpenter/crds/`
8. Starts the local controller with:

   ```sh
   go run ./cmd/controller/main.go
   ```

9. Waits for the controller to create its template node pools before running tests

The four live cases are:

- `TestOnDemandAMD64` — `e2-small`
- `TestSpotAMD64` — `e2-small`
- `TestOnDemandARM64` — `t2a-standard-1`
- `TestSpotARM64` — `t2a-standard-1`

Each test creates its own `GCENodeClass`, `NodePool`, and workload so cases do not interfere with each other.

## Cleanup guarantees

Cleanup uses a stack so every successful setup step registers a matching teardown step immediately. Teardown runs even when setup fails, a test fails, the suite panics, or the process is interrupted with SIGINT/SIGTERM.

Cleanup order is effectively reverse-creation and includes:

1. Stop the local Karpenter subprocess
2. Delete Karpenter `NodeClaims`, `NodePools`, and `GCENodeClasses`
3. Delete the test namespaces and workload identity service account
4. Delete the GKE cluster and wait until it is gone
5. Retry subnetwork and VPC deletion until GKE has released dependencies
6. Remove project IAM bindings and workload identity bindings
7. Delete the suite-managed Google service account
8. List clusters, networks, subnetworks, firewall rules, routes, instances, service accounts, and IAM members matching the prefix and fail if anything remains

## Notes

- These tests create billable GCP resources.
- The cluster control plane is zonal to keep costs down.
- The controller runs with a temporary key for the suite-managed `${prefix}-karpenter` service account, not the e2e runner key and not an in-cluster secret.
- `PROPOSAL.md` is intentionally left unchanged as design history.

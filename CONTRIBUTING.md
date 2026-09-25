# Contributing

## Design Proposals

For significant changes — architectural redesigns, new API fields, operator-visible behaviour changes, or multi-phase work — opening a proposal before writing code helps align on the approach early and produces a useful design record.

### When to write a proposal

Write a proposal when your change involves any of the following:

- Architectural redesign or replacement of a major subsystem
- New `GCENodeClass` fields, new CRDs, or changes to existing API fields
- Multi-phase implementation spanning several PRs
- Changes that require operator action on upgrade (new required config, migration steps)
- Large changes from external contributors (strongly encouraged)

A proposal is **not required** for bug fixes, small flag additions, doc-only changes, or refactors that do not change observable behaviour.

### How to submit a proposal

1. Copy [`proposals/0000-template.md`](proposals/0000-template.md) to `proposals/NNNN-short-title.md`, where `NNNN` is the next available four-digit number.
2. Fill in all **required** sections (Summary, Motivation, Proposal). Optional sections can be omitted or marked N/A.
3. Open a draft PR with the proposal. The `Status` field should be `Draft` while the approach is still being formed. Update it to `Provisional` when you are ready for initial feedback.
4. Iterate in review. When the approach is agreed by maintainers, update `Status` to `Implementable`.
5. Implementation PRs reference the proposal via the "Related proposal" field in the PR template.

Proposals are reviewed by project maintainers via GitHub PR review. Assign the PR for review or mention `@cloudpilot-ai/karpenter-gcp` when the proposal is ready for feedback.

### Status values

| Status | Meaning |
|--------|---------|
| `Draft` | Work in progress, not ready for review |
| `Provisional` | Ready for feedback, approach not yet agreed |
| `Implementable` | Approach agreed; implementation may proceed |
| `Implemented` | Fully implemented and merged |
| `Deferred` | Accepted but not currently scheduled |
| `Rejected` | Reviewed and rejected |
| `Withdrawn` | Author withdrew the proposal |
| `Replaced` | Superseded by a newer proposal (include link) |

---

## Development

This doc explains how to set up a development environment so you can get started contributing to this project

### Prerequisites

1. [`go`](https://golang.org/doc/install): For building the project
1. [`git`](https://help.github.com/articles/set-up-git/): For source control
1. [`gcloud`](https://cloud.google.com/sdk/docs/install): For Interacting with the Google Cloud Platform
1. [`golangci-lint`](https://golangci-lint.run/welcome/install/): Required by `make presubmit` — install via your system package manager, the official install script, or a global `go install`; it is not managed via this repo's `tools/go.mod` due to transitive dependency conflicts

This guides breaks down into two parts 
1. Setting up the GKE cluster - If you have already setup the GKE cluster, you can skip this part
2. Setting up the development environment - This part is required for all the contributors

### Setting up the GKE cluster

1. First Go ahead and create new project in the Google Cloud Platform and enable billing for the project if you don't have it already enabled. You can follow the below links as guide on how to create a new project and enable billing for the project
    - [Creating the project](https://cloud.google.com/resource-manager/docs/creating-managing-projects)
    - [Enable billing for given project](https://cloud.google.com/billing/docs/how-to/modify-project)


2. Once you have created the project enable the below apis 

    ```
    gcloud services enable compute.googleapis.com
    gcloud services enable container.googleapis.com
    ```

3. Next go ahead and create the service account in the project and assign the following role to the service account

    - Compute Admin
    - Kubernetes Engine Admin
    - Monitoring Admin
    - Service Account User


4. Once you have assigned the roles to the service account, go ahead and create a new key for the service account by following this link as a guide [Creating and managing service account keys](https://cloud.google.com/iam/docs/creating-managing-service-account-keys)


    > Note : Make sure you have the service account key stored in a secure location and do not share it with anyone.

5. Now configure the gcloud sdk to use the service account key by running the below command

    ```
    gcloud auth activate-service-account --key-file /path/to/key.json
    ```

6. First configure the required environment variable using the below command

    ```bash
    export PROJECT_ID=<project-id>
    export CLUSTER_NAME=<cluster-name>
    export REGION=<region>
    ```

7. Next create a GKE cluster using the below command
    ```
    gcloud container clusters create $CLUSTER_NAME --location=$REGION --num-nodes 1 --machine-type=n1-standard-1 --disk-size=10 --project $PROJECT_ID 
    ```


8. Once the cluster is created, go ahead and get the credentials for the cluster by running the below command

    ```
    gcloud container clusters get-credentials $CLUSTER_NAME --location=$REGION --project $PROJECT_ID
    ```

9. Now export the generated kubeconfig file to the `KUBECONFIG` environment variable by running the below command


    ```
    export KUBECONFIG=~/.kube/config
    ```


10. Next export the service account key to the `GOOGLE_APPLICATION_CREDENTIALS` environment variable by running the below command


    ```
    export GOOGLE_APPLICATION_CREDENTIALS="/path/to/key.json"
    ```

### Setting up the development environment

1. If you already setup the PROJECT_ID ,REGION and CLUSTER_NAME environment variables in the while setting up the GKE cluster, you can skip this step. If not, go ahead and export the below environment variables

    ```bash
    export PROJECT_ID=<project-id>
    export REGION=<region>
    export CLUSTER_NAME=<cluster-name>
    ```

2. Once you have exported the environment variables, go ahead and install the CRDs for the karpenter by running the below command

    ```
    kubectl apply -f charts/karpenter/crds/
    ```

3. Once you have installed the CRDs, go ahead and install the karpenter by running the below command
    ```
    make run
    ```

### End-to-end tests

E2e tests run against a real GKE cluster. The cluster is **not** torn down between runs — it is reused across test sessions to save setup time.

#### Prerequisites

Run from the checkout containing the deployed controller. Export these variables (or load them from your local `.envrc` with `direnv`) before using the e2e Make targets:

```bash
export E2E_PROJECT_ID=<gcp-project-id>
export E2E_LOCATION=<zone-or-region>  # e.g. us-central1-f or us-central1
export E2E_REGION=<matching-region>  # needed by setup/deploy; e.g. us-central1
export KUBECONFIG=/path/to/your/kubeconfig
export E2E_SA_PATH=/path/to/service-account-key.json  # omit if using application-default credentials
```

`E2E_PROJECT_ID` and `E2E_LOCATION` are required; Make rejects empty values. `KUBECONFIG` should point to the intended cluster (otherwise the usual kubectl default applies). With `E2E_SA_PATH`, Make sets `GOOGLE_APPLICATION_CREDENTIALS` for the tests; without it, use an authenticated `gcloud` session and application-default credentials. `E2E_REGION` defaults to `us-central1` for setup/deploy regardless of location, so set it explicitly for other regions. It is not used by `e2e-tests`.

#### Required permissions

The service account pointed to by `E2E_SA_PATH` must have the following IAM roles on the project:

| Role | Why needed |
|------|------------|
| `roles/container.admin` | Create/delete/describe GKE clusters |
| `roles/compute.networkAdmin` | Create/delete VPC and subnet |
| `roles/compute.viewer` | List instances and disks (e2e-check-clean) |
| `roles/iam.serviceAccountAdmin` | Create/delete the karpenter service account |
| `roles/resourcemanager.projectIamAdmin` | Bind roles to the karpenter service account |
| `roles/artifactregistry.admin` | Create/delete Artifact Registry repo and push images |

#### One-time cluster setup

```bash
make e2e-setup
```

This idempotently creates a GKE cluster, VPC, subnet, Cloud Router, Cloud NAT, service account, IAM bindings, Artifact Registry repo, and deploys karpenter via Helm.

Cloud NAT is required for the `networking` suite: nodes provisioned with `enableExternalIPAccess: false` have no public IP and need NAT for outbound internet access.

#### Deploy a new controller image

```bash
make e2e-deploy
```

Builds the controller image with `ko` and runs `helm upgrade --install`. Before deploying, it removes only resources labeled `karpenter-e2e/owned=true` and the dedicated test namespace; inspect legacy unlabeled leftovers manually.

#### Run end-to-end tests

Before choosing concurrency, check available addresses, SSD, regional CPUs, and all-regions CPUs. `GINKGO_PROCS` caps **concurrent specs across the whole run**, not per feature.

```bash
make e2e-tests                                                 # standard: all non-GPU features
make e2e-tests E2E_SELECTION=drift GINKGO_PROCS=1              # one feature
make e2e-tests E2E_SELECTION=drift,storage GINKGO_PROCS=2 \
  E2E_REPORT=.pi/drift-storage.md                               # custom ignored report
make e2e-tests E2E_SELECTION=all GINKGO_PROCS=4                # includes GPU
```

Inputs for `make e2e-tests` (in addition to the prerequisites above):

| Input | Default | Purpose |
|-------|---------|---------|
| `E2E_SELECTION` | `standard` | `standard` (non-GPU), `gpu`, `all`, `provisioning`, one feature directory, or a comma-separated list such as `drift,storage`. |
| `GINKGO_PROCS` | `4` | Global limit on concurrently running specs; choose based on quota. |
| `E2E_REPORT` | `e2e-report.md` | Markdown report path; the companion controller log uses the same basename with `.karpenter.log`. Keep a nonempty path to retain commit checks and reporting. |
| `E2E_LOCK_ID` | `username@hostname:pid-<runner PID>` | Lease holder identity; override when a stable CI identity is useful. |
| `E2E_PREFIX` | `karpenter-e2e` | Base name for the default cluster and pods range. |
| `E2E_CLUSTER_NAME` / `E2E_PODS_RANGE` | `<prefix>-cluster` / `<prefix>-pods` | Override when testing an existing cluster with different names. |
| `E2E_KARPENTER_NAMESPACE` / `E2E_KARPENTER_DEPLOYMENT` | `karpenter-system` / `karpenter` | Override the controller target for commit checks, logs, and the Lease namespace. |
| `E2E_PRESET` | unset | Legacy alias for `E2E_SELECTION`; do not set both. |

`SUITE` and `FOCUS` apply only to the separate `e2e-test` target, not `e2e-tests`. Explicit GPU selections (`gpu`, `all`, or a list containing `gpu`) need enough GPU quota and capacity.

The runner verifies the deployed controller matches the test checkout, acquires a Kubernetes Lease (overlapping test runs fail fast), and writes the ignored report and controller log. It releases the Lease on normal exit or interruption; after a forced kill, the Lease expires within ten minutes. Setup, deploy, and cleanup are **not** covered by this test Lease. Inspect the report and log even when tests fail; a startup failure may leave no report.

For one-spec debugging, `make e2e-test SUITE=provisioning FOCUS="amd64 on-demand"` remains available, but it bypasses the Lease and report: do not use it on a shared cluster. Run a focused Ginkgo spec through `hack/e2e-runner` instead if locking is required.

#### On-demand PR e2e

The `/e2e` pull-request comment workflow is currently a placeholder; run local e2e using the commands above.

#### Tear down all e2e infrastructure

```bash
make e2e-teardown
```

#### Check for orphaned resources (without deleting)

```bash
make e2e-check-clean
```

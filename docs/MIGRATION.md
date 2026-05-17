# Migration Guide

This document tracks breaking changes and required actions when upgrading karpenter-provider-gcp. Check this file on every upgrade. The canonical IAM permission list is in [`deploy/iam/karpenter-controller-role.yaml`](../deploy/iam/karpenter-controller-role.yaml).

---

## Unreleased

Before running any command below, set:

```sh
export PROJECT_ID=<your-project-id>
export GSA_NAME=karpenter-gsa   # the name of your Karpenter controller GCP service account
```

### Replace `roles/compute.admin` + `roles/container.admin` with a minimal custom role

**Action required for all existing installations.**

Previous installation instructions granted two broad predefined roles to the controller SA:

- `roles/compute.admin` — full write access to all Compute Engine resources in the project
- `roles/container.admin` — full admin access to all GKE resources in the project

These are far broader than required. Create a minimal custom role instead:

```sh
gcloud iam roles create karpenter_controller \
    --project=$PROJECT_ID \
    --file=deploy/iam/karpenter-controller-role.yaml

gcloud projects add-iam-policy-binding $PROJECT_ID \
    --member="serviceAccount:$GSA_NAME@$PROJECT_ID.iam.gserviceaccount.com" \
    --role="projects/$PROJECT_ID/roles/karpenter_controller"
```

Then remove the old broad bindings:

```sh
gcloud projects remove-iam-policy-binding $PROJECT_ID \
    --member="serviceAccount:$GSA_NAME@$PROJECT_ID.iam.gserviceaccount.com" \
    --role="roles/compute.admin"

gcloud projects remove-iam-policy-binding $PROJECT_ID \
    --member="serviceAccount:$GSA_NAME@$PROJECT_ID.iam.gserviceaccount.com" \
    --role="roles/container.admin"
```

### Scope `iam.serviceAccountUser` to the node SA (not project-wide)

**Action required if you followed the previous installation guide.**

The previous guide granted `roles/iam.serviceAccountUser` at project level (actAs any SA in the project). This should be scoped to only the SA(s) Karpenter attaches to nodes. Add the scoped binding before removing the broad one to avoid a permission gap:

```sh
# Add the SA-scoped binding first to avoid a permission gap.
# Use the Compute Engine default SA or your custom node SA — see install guide Step 1.
export NODE_SA_EMAIL=<your-node-sa>@<your-project-id>.iam.gserviceaccount.com
gcloud iam service-accounts add-iam-policy-binding $NODE_SA_EMAIL \
    --role roles/iam.serviceAccountUser \
    --member "serviceAccount:$GSA_NAME@$PROJECT_ID.iam.gserviceaccount.com"

# Remove the broad project-wide binding.
gcloud projects remove-iam-policy-binding $PROJECT_ID \
    --member="serviceAccount:$GSA_NAME@$PROJECT_ID.iam.gserviceaccount.com" \
    --role="roles/iam.serviceAccountUser"
```

If you override the node SA via `GCENodeClass.spec.serviceAccount` or `--node-pool-service-account`, repeat the `add-iam-policy-binding` step for each SA you use.

> **Note:** When live GCP pricing is added (see [#231](https://github.com/cloudpilot-ai/karpenter-provider-gcp/pull/231)), `cloudbilling.services.get` and `cloudbilling.skus.list` will need to be added to `deploy/iam/karpenter-controller-role.yaml`. This guide will be updated at that point.

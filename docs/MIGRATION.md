# Migration Guide

This document tracks changes that require action when upgrading karpenter-provider-gcp. Check this file on every upgrade.

---

## IAM changes

Karpenter's GCP IAM permissions are defined in [`deploy/iam/karpenter-controller-role.yaml`](../deploy/iam/karpenter-controller-role.yaml). That file is the canonical reference — when permissions change, this document records what changed and why, so you know whether your deployment needs updating.

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

These are far broader than required and violate the principle of least privilege.

**Replacement:** Create a minimal custom role from [`deploy/iam/karpenter-controller-role.yaml`](../deploy/iam/karpenter-controller-role.yaml):

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

**`iam.serviceAccountUser` — scope change.** The previous guide granted `roles/iam.serviceAccountUser` at project level (actAs any SA in the project). Scope it to just the node SA instead. Add the SA-scoped binding before removing the broad one to avoid a permission gap:

```sh
# Add the SA-scoped binding first (before removing the broad one) to avoid a permission gap.
# To find your node SA, see the install guide Step 1.
export NODE_SA_EMAIL=<your-node-sa>@<your-project-id>.iam.gserviceaccount.com
gcloud iam service-accounts add-iam-policy-binding $NODE_SA_EMAIL \
    --role roles/iam.serviceAccountUser \
    --member "serviceAccount:$GSA_NAME@$PROJECT_ID.iam.gserviceaccount.com"

# Now remove the overly broad project-wide binding.
gcloud projects remove-iam-policy-binding $PROJECT_ID \
    --member="serviceAccount:$GSA_NAME@$PROJECT_ID.iam.gserviceaccount.com" \
    --role="roles/iam.serviceAccountUser"
```

### Cloud Billing API permissions — not currently required

The pricing provider currently fetches prices from a third-party CSV source and does not call the GCP Cloud Billing API. No billing permissions are needed.

> **Note:** When live GCP pricing is added (see [#231](https://github.com/cloudpilot-ai/karpenter-provider-gcp/pull/231)), `cloudbilling.services.get` and `cloudbilling.skus.list` will need to be added to `deploy/iam/karpenter-controller-role.yaml`. This migration guide will be updated at that point.

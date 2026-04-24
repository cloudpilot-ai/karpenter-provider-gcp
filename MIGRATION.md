# Migration Guide

## Upgrading to vNext — Template pool elimination

Karpenter no longer creates or relies on `karpenter-default`, `karpenter-ubuntu`,
`karpenter-cos-arm64`, or `karpenter-ubuntu-arm64` node pools. The upgrade itself requires
no action — Karpenter discovers an existing RUNNING cluster pool automatically.

After confirming provisioning works correctly with the new version, delete all four legacy
pools at your own pace:

```bash
for pool in karpenter-ubuntu karpenter-cos-arm64 karpenter-ubuntu-arm64 karpenter-default; do
  gcloud container node-pools delete "$pool" --cluster=<CLUSTER> --region=<REGION> --quiet
done
```

> **Note:** the new last-resort fallback pool is named `karpenter-fallback` (not
> `karpenter-default`), so deleting all four names above is safe and unambiguous.

Rolling back to the previous version will re-create the legacy pools automatically.

---

## Upgrading to vNext — GC controller and cluster identity labels

### Background

This release introduces a garbage-collection controller that automatically deletes GCE VM
instances with no corresponding NodeClaim (orphaned instances that accumulate after missed
deletes, controller restarts, or direct NodeClaim deletions).

Karpenter identifies instances that belong to this cluster using two GCE labels:

- `goog-k8s-cluster-name` — already present on all Karpenter-managed instances; used as a
  server-side filter in GCE API calls.
- `goog-k8s-cluster-location` — new in this release; stamped on every instance at creation
  time and checked in-process to distinguish clusters that share the same name in different
  GCP locations.

Instances with `goog-k8s-cluster-location` present are eligible for automatic GC.
Instances without it — created by an older Karpenter version — remain in the instance
cache (so Karpenter retains control of them) but are **never** touched by the GC
controller. This means upgrading is safe: no nodes are deleted on upgrade, and existing
nodes stay fully managed until they are naturally replaced by a newer Karpenter version
that stamps the label.

The upgrade itself requires no action. The steps below are **optional** and only relevant
if you want to clean up orphaned instances that may have accumulated before this release
(see #242). Future orphaned instances on new-style nodes are handled automatically.

---

### Step 1 (optional) — rotate live nodes

Trigger a rolling replacement of your NodePools so that every replacement instance is
stamped with `goog-k8s-cluster-location`. After this step, any remaining instance that
still lacks the label is confirmed to have no live workload and is safe to treat as an
orphan.

```bash
kubectl annotate nodepool <NODEPOOL_NAME> "karpenter.k8s.gcp/force-rollout=$(date +%s)" --overwrite
```

Repeat for each NodePool. Wait for all nodes to finish replacing before proceeding.

---

### Step 2 (optional) — find and delete orphaned instances

List all instances with `goog-k8s-cluster-name` but without `goog-k8s-cluster-location`:

```bash
gcloud compute instances list \
  --project=PROJECT_ID \
  --filter="labels.goog-k8s-cluster-name=CLUSTER_NAME AND -labels.goog-k8s-cluster-location:*" \
  --format="table(name,zone,status,creationTimestamp)"
```

If step 1 completed successfully, every instance in this list is an orphan. Delete them:

```bash
gcloud compute instances list \
  --project=PROJECT_ID \
  --filter="labels.goog-k8s-cluster-name=CLUSTER_NAME AND -labels.goog-k8s-cluster-location:*" \
  --format="value(name,zone)" | \
while IFS=$'\t' read -r name zone; do
  gcloud compute instances delete "$name" --zone="$zone" --project=PROJECT_ID --quiet
done
```

---

## Upgrading to v0.2.0 — instance family label change

See [CHANGELOG.md](CHANGELOG.md#v020) for the breaking change to `karpenter.k8s.gcp/instance-family`
requirements and the corresponding upgrade steps.

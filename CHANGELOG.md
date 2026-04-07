# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),

## Unreleased

### New features

- Added a garbage-collection controller (`instance.garbagecollection`) that periodically
  detects and deletes GCE VM instances with no corresponding NodeClaim, preventing orphaned
  instances from accumulating after missed deletes or controller restarts.
- Added `goog-k8s-cluster-location` GCE label to instances at creation time. The GC
  controller and the instance cache only operate on instances that carry this label, so
  they reliably scope to the correct cluster even when multiple clusters share the same name
  in different GCP locations.

### Migration guide — cleaning up pre-existing orphaned instances

The new GC controller only manages instances it can positively identify as belonging to
this cluster (i.e., those that carry the `goog-k8s-cluster-location` label stamped by this
version of Karpenter). Instances created by earlier versions of Karpenter have no such
label and are intentionally left untouched by the GC controller.

Because older versions lacked the GC controller entirely (see #242), you may already have
orphaned instances that accumulated before this upgrade. This is a **one-time** cleanup;
once all nodes have been cycled through the new version, future orphans are handled
automatically.

**Step 1 — find orphaned legacy instances**

List all GCE instances that have a `goog-k8s-cluster-name` label (i.e., were created by
Karpenter) but no `goog-k8s-cluster-location` label (i.e., created before this version):

```bash
gcloud compute instances list \
  --project=PROJECT_ID \
  --filter="labels.goog-k8s-cluster-name=CLUSTER_NAME AND -labels.goog-k8s-cluster-location:*" \
  --format="table(name,zone,status,creationTimestamp)"
```

**Step 2 — verify they are truly orphaned**

Cross-check against your NodeClaims. A running instance is an orphan only if there is no
NodeClaim whose `.status.providerID` matches `gce://PROJECT_ID/ZONE/INSTANCE_NAME`:

```bash
kubectl get nodeclaims -o jsonpath='{range .items[*]}{.status.providerID}{"\n"}{end}'
```

**Step 3 — delete confirmed orphans**

```bash
gcloud compute instances delete INSTANCE_NAME --zone=ZONE --project=PROJECT_ID
```

Or to delete all results from step 1 in bulk (review the list first):

```bash
gcloud compute instances list \
  --project=PROJECT_ID \
  --filter="labels.goog-k8s-cluster-name=CLUSTER_NAME AND -labels.goog-k8s-cluster-location:*" \
  --format="value(name,zone)" | \
while IFS=$'\t' read -r name zone; do
  gcloud compute instances delete "$name" --zone="$zone" --project=PROJECT_ID --quiet
done
```

**Step 4 — cycle remaining live nodes (optional but recommended)**

To migrate live nodes to the new label scheme so the GC controller can fully manage them,
trigger a rolling replacement by updating or re-applying your NodePools. Karpenter will
drift-replace each node, and replacement instances will carry the `goog-k8s-cluster-location`
label automatically.

## v0.2.0

### Breaking Changes

- `karpenter.k8s.gcp/instance-family` requirements now match the **machine type prefix** (the part before the first `-`), not the full family+shape.
  - **Before** (worked): `["n4-standard", "n2-standard"]`
  - **Now** (required): `["n4", "n2"]`
  - `e2` is unchanged (e.g. `e2-medium` still has family `e2`).

If you upgrade to v0.2.0 and Karpenter suddenly stops finding instance types, check any existing NodePools that constrain `karpenter.k8s.gcp/instance-family` and update values accordingly.

### Upgrade guide

Update any NodePool `requirements` that use `karpenter.k8s.gcp/instance-family` to use the prefix (e.g. `n4`, `n2`), not the combined family+shape (e.g. `n4-standard`, `n2-standard`).

Before:

```yaml
requirements:
  - key: "karpenter.k8s.gcp/instance-family"
    operator: In
    values: ["n4-standard", "n2-standard", "e2"]
```

After:

```yaml
requirements:
  - key: "karpenter.k8s.gcp/instance-family"
    operator: In
    values: ["n4", "n2", "e2"]
```

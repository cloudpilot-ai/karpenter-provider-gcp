# Migration Guide

## Upgrading to vNext — GC controller and `goog-k8s-cluster-location` label

### Background

This release introduces a garbage-collection controller that automatically deletes GCE VM
instances with no corresponding NodeClaim (orphaned instances that accumulate after missed
deletes, controller restarts, or direct NodeClaim deletions).

The GC controller identifies instances that belong to this cluster via a new GCE label,
`goog-k8s-cluster-location`, stamped on every instance at creation time. Instances without
this label — i.e., those created by an older version of Karpenter — are intentionally
ignored by the GC controller and must be handled manually as part of this migration.

Because older versions lacked the GC controller entirely (see #242), you may already have
orphaned instances. The steps below cover both concerns: migrating live nodes to the new
label scheme, and cleaning up any pre-existing orphans.

---

### Step 1 — rotate all live nodes

After upgrading Karpenter, trigger a rolling replacement of all existing nodes so that each
replacement instance is stamped with the `goog-k8s-cluster-location` label. Until a node
has been replaced, it remains outside the GC controller's scope.

The easiest way is to annotate each NodePool to force drift:

```bash
kubectl annotate nodepools --all karpenter.sh/do-not-disrupt- --overwrite
kubectl rollout restart deployment -n karpenter karpenter  # if not already restarted by upgrade
```

Or trigger drift explicitly by bumping a NodePool label or annotation:

```bash
kubectl annotate nodepool <NODEPOOL_NAME> "karpenter.k8s.gcp/force-rollout=$(date +%s)" --overwrite
```

Karpenter will cordon, drain, and replace each node. Replacement instances will carry
`goog-k8s-cluster-location` automatically. Wait for all NodePools to finish rolling before
proceeding.

---

### Step 2 — find instances without `goog-k8s-cluster-location`

Once live nodes have been rotated, any remaining instances with `goog-k8s-cluster-name` but
without `goog-k8s-cluster-location` are candidates for cleanup. List them:

```bash
gcloud compute instances list \
  --project=PROJECT_ID \
  --filter="labels.goog-k8s-cluster-name=CLUSTER_NAME AND -labels.goog-k8s-cluster-location:*" \
  --format="table(name,zone,status,creationTimestamp)"
```

---

### Step 3 — verify they are truly orphaned

Cross-check against live NodeClaims. An instance is an orphan only if no NodeClaim has a
`.status.providerID` matching `gce://PROJECT_ID/ZONE/INSTANCE_NAME`:

```bash
kubectl get nodeclaims -o jsonpath='{range .items[*]}{.status.providerID}{"\n"}{end}'
```

After step 1 completes successfully, any instance still appearing in step 2's list should
have no matching NodeClaim and is safe to delete.

---

### Step 4 — delete confirmed orphans

Delete individually:

```bash
gcloud compute instances delete INSTANCE_NAME --zone=ZONE --project=PROJECT_ID
```

Or bulk-delete all results from step 2 (review the list from step 2 before running):

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

This is a one-time operation. Once all nodes have been rotated and orphans cleaned up, the
GC controller handles any future orphaned instances automatically.

---

## Upgrading to v0.2.0 — instance family label change

See [CHANGELOG.md](CHANGELOG.md#v020) for the breaking change to `karpenter.k8s.gcp/instance-family`
requirements and the corresponding upgrade steps.

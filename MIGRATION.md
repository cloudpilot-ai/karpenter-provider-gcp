# Migration Guide

## Upgrading to vNext — network config, tags, and service account changes

### Network interfaces

Karpenter now builds the primary network interface from the cluster API (`cluster.NetworkConfig`) instead of copying it from a GKE node pool template. The network, subnetwork, and pod CIDR range are read directly from the cluster. NodeClass overrides (`subnetwork`, `enableExternalIPAccess`, `subnetRangeName`) continue to work as before.

**Multi-interface:** Secondary interfaces (`networkInterfaces[1+]`) are supported but require an explicit `subnetwork` — entries without one are silently skipped. Previously, secondary interfaces were cloned from the node pool template; if they relied on template-inherited subnetworks, add the subnetwork explicitly to the NodeClass.

**Cluster-level private nodes** (`EnablePrivateNodes: true`) are now detected automatically — no NodeClass override is needed.

### Network tags

Previously, Karpenter merged all tags from the bootstrap node pool template with `spec.networkTags` in NodeClass. Now only two sources are used:

- The cluster-wide GKE tag `gke-<cluster-name>-node` (always present)
- `spec.networkTags` from NodeClass

**Action required:** If your GKE node pool template carried additional tags used in firewall rules (e.g. pool-specific tags), add them explicitly to `spec.networkTags` in the relevant NodeClass.

### Node service account

The service account resolution order is now:

1. `spec.serviceAccount` in GCENodeClass
2. `--node-pool-service-account` operator flag / `NODE_POOL_SERVICE_ACCOUNT` env var
3. No explicit SA — GCE uses the project's Compute Engine default service account (`<project-number>-compute@developer.gserviceaccount.com`), the same default GKE applies when no SA is specified at node pool creation

The fallback to the template's service account list has been removed. If your NodeClass and operator flag are both unset, provisioned nodes will use the Compute Engine default SA. This matches GKE's own default, but GKE recommends using a dedicated SA with minimal permissions ([`roles/container.nodeServiceAccount`](https://cloud.google.com/kubernetes-engine/docs/how-to/hardening-your-cluster#use_least_privilege_sa)) for production clusters. Set `NODE_POOL_SERVICE_ACCOUNT` or `spec.serviceAccount` accordingly.

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

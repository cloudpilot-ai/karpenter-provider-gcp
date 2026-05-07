# Migration Guide

## Unreleased

### Pricing API: Cloud Billing Catalog

Karpenter now fetches machine type prices live from the [GCP Cloud Billing Catalog API](https://cloud.google.com/billing/docs/reference/rest) instead of using a bundled static CSV. This improves accuracy and keeps prices current without requiring a new Karpenter release.

**Action required:** enable the Cloud Billing API in your GCP project:

```sh
gcloud services enable cloudbilling.googleapis.com --project=<your-project-id>
```

No new IAM roles are needed — listing public billing SKUs requires only valid GCP credentials, which the Karpenter service account already has.

---

## v0.3.0

### CRDs moved to a separate Helm chart (`karpenter-crd`)

CRDs are now managed via a dedicated `karpenter-crd` Helm chart. Users upgrading from v0.2.x must install this chart and, if CRDs were previously installed by the single `karpenter` chart, adopt them into the new release first.

See [Upgrading](docs/getting-started/upgrading.md) for the full procedure.

---

### Network config and tags

#### Network interfaces

Karpenter now builds the primary network interface from the cluster API (`cluster.NetworkConfig`) instead of copying it from a GKE node pool template. The network, subnetwork, and pod CIDR range are read directly from the cluster.

The `networkConfig` API has been redesigned to mirror the `network_config` block in Terraform's `google_container_node_pool` resource, making it immediately familiar to operators who configure GKE node pools with Terraform:

- `networkConfig.enablePrivateNodes` — whether provisioned nodes have internal IPs only (mirrors `network_config.enable_private_nodes`)
- `networkConfig.subnetwork` — primary subnetwork override (mirrors `network_config.subnetwork`)
- `networkConfig.additionalNetworkInterfaces` — secondary interfaces, each requiring a `subnetwork` and optionally a `network` (mirrors `network_config.additional_node_network_configs`)

**Action required if you used `networkConfig.networkInterfaces` (this field existed only in unreleased `main` builds — no released version ever shipped it):**

```yaml
# Before
networkConfig:
  networkInterfaces:
    - enableExternalIPAccess: false
      subnetwork: regions/us-central1/subnetworks/my-subnet

# After
networkConfig:
  enablePrivateNodes: true
  subnetwork: regions/us-central1/subnetworks/my-subnet
```

For secondary interfaces (previously `networkInterfaces[1+]`):

```yaml
# Before
networkConfig:
  networkInterfaces:
    - {}
    - subnetwork: regions/us-central1/subnetworks/secondary

# After
networkConfig:
  additionalNetworkInterfaces:
    - subnetwork: regions/us-central1/subnetworks/secondary
```

**Action required if you used `networkConfig.networkInterface` (the intermediate wrapper form — also only in unreleased `main` builds):**

```yaml
# Before
networkConfig:
  networkInterface:
    enableExternalIPAccess: false
    subnetwork: regions/us-central1/subnetworks/my-subnet

# After
networkConfig:
  enablePrivateNodes: true
  subnetwork: regions/us-central1/subnetworks/my-subnet
```

**Cluster-level private nodes** (`EnablePrivateNodes: true`) are now detected automatically — no NodeClass override is needed.

#### New IAM permission: `container.clusters.get`

Karpenter now reads the cluster API at provisioning time (`container.projects.locations.clusters.get`) to derive network configuration. This requires the `container.clusters.get` IAM permission on the Karpenter service account.

**Action required if you use a custom minimal IAM role** (not `roles/container.admin`): add `container.clusters.get` to the role. No action is needed if you use the predefined `roles/container.admin` role, which already includes this permission.

#### Network tags

Previously, Karpenter merged all tags from the bootstrap node pool template with `spec.networkTags` in NodeClass. Now only two sources are used:

- The cluster-wide GKE tag `gke-<cluster-name>-node` (always present)
- `spec.networkTags` from NodeClass

**Action required:** If your GKE node pool template carried additional tags used in firewall rules (e.g. pool-specific tags), add them explicitly to `spec.networkTags` in the relevant NodeClass.

---

### GC controller and cluster identity labels

#### Background

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

#### Step 1 (optional) — rotate live nodes

Trigger a rolling replacement of your NodePools so that every replacement instance is
stamped with `goog-k8s-cluster-location`. After this step, any remaining instance that
still lacks the label is confirmed to have no live workload and is safe to treat as an
orphan.

```bash
kubectl annotate nodepool <NODEPOOL_NAME> "karpenter.k8s.gcp/force-rollout=$(date +%s)" --overwrite
```

Repeat for each NodePool. Wait for all nodes to finish replacing before proceeding.

---

#### Step 2 (optional) — find and delete orphaned instances

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

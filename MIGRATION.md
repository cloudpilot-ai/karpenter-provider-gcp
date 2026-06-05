# Migration Guide

> For a full list of changes per release, see [GitHub Releases](https://github.com/cloudpilot-ai/karpenter-provider-gcp/releases).
> This file documents only breaking changes and steps required when upgrading.

## Unreleased

---

## v0.4.0

### `kubeletConfiguration` fields now take effect

`spec.kubeletConfiguration` fields on `GCENodeClass` that were previously accepted by the CRD but silently dropped at provisioning time are now applied to the kubelet on every Karpenter-provisioned node and to Karpenter's scheduler bin-packing. The previously honoured fields (`maxPods`) are unchanged.

Newly honoured fields:

- `systemReserved`, `kubeReserved` — reservations passed to the kubelet via `--config` and used by the scheduler to compute allocatable.
- `evictionHard`, `evictionSoft`, `evictionSoftGracePeriod`, `evictionMaxPodGracePeriod` — passed to the kubelet. For the scheduler's overhead, only `evictionHard.memory.available` and `evictionHard.nodefs.available` are modelled (matches `aws/karpenter-provider-aws` and the [Kubernetes node-pressure-eviction docs](https://kubernetes.io/docs/tasks/administer-cluster/reserve-compute-resources/#eviction-thresholds)). `evictionSoft` is a warning threshold and is enforced by the kubelet at runtime but does not reduce scheduler allocatable.
- `imageGCHighThresholdPercent`, `imageGCLowThresholdPercent`, `cpuCFSQuota`, `clusterDNS`, `podsPerCore` — passed to the kubelet. `podsPerCore` additionally caps `node.status.capacity.pods` at `podsPerCore × cpus`.

**Action required:** audit existing `GCENodeClass` objects that set any of these fields. The values will now take effect on the next node provision, and a change to any field triggers Karpenter's standard drift-driven node rotation.

The CRD value schema for `systemReserved`, `kubeReserved`, `evictionHard`, and `evictionSoft` is now validated against the `resource.Quantity` regex (and percentage syntax for the eviction fields). Existing values that already parsed as `resource.Quantity` are unaffected.

---
### GPU node provisioning (`gpuDriverVersion`)

Karpenter now automatically sets the two node labels required by the GKE GPU software stack
at boot time:

- `cloud.google.com/gke-accelerator=<type>` — required by the NVIDIA device plugin DaemonSet.
- `cloud.google.com/gke-gpu-driver-version=<value>` — read by the GKE GPU driver installer.

The driver version is controlled by the new `spec.gpuDriverVersion` field on `GCENodeClass`
(default: `"default"`, matching GKE's native behaviour).

**Action required if you set `cloud.google.com/gke-gpu-driver-version` via NodePool
`spec.template.spec.labels`:** remove the label from the NodePool and set
`spec.gpuDriverVersion` on `GCENodeClass` instead — it is now injected at boot time,
before the driver installer runs.

```yaml
# Before — NodePool:
spec:
  template:
    spec:
      labels:
        cloud.google.com/gke-gpu-driver-version: default

# After — remove the NodePool label; set in GCENodeClass:
spec:
  gpuDriverVersion: default
```

**Node rotation on upgrade:** `GCENodeClassHashVersion` is bumped to `v4`. On upgrade, Karpenter detects that all existing `GCENodeClass` objects carry a stale hash version and triggers a rolling node replacement for every affected NodePool. This is a one-time, controlled rotation — nodes are replaced gradually, not all at once.

### Image selection redesigned — `alias` deprecated

`imageSelectorTerms` now supports structured `family`, `channel`, and `version` fields. The old `alias` string field is deprecated and will be removed in a future release — **migrate at your own pace, but do not leave it in place indefinitely.**

Replace `alias` with the equivalent structured form:

```yaml
# Before
imageSelectorTerms:
  - alias: ContainerOptimizedOS@latest

# After — follow the cluster's enrolled GKE release channel
imageSelectorTerms:
  - family: ContainerOptimizedOS
    channel: cluster
```

```yaml
# Before
imageSelectorTerms:
  - alias: ContainerOptimizedOS@125.19216.104.126

# After — pin to a specific COS milestone
imageSelectorTerms:
  - family: ContainerOptimizedOS
    version: "125.19216.104.126"
```

```yaml
# Before
imageSelectorTerms:
  - alias: Ubuntu@v20260416

# After — pin to a specific Ubuntu 24.04 date build
imageSelectorTerms:
  - family: Ubuntu2404
    version: "v20260416"
```

See [Image selection](docs/image-selection.md) for the full field reference and [Image management](docs/image-management.md#legacy-image-alias-format) for the complete migration table.

**IAM note:** GKE release-channel image resolution calls the `projects.locations.getServerConfig` API method. Custom IAM roles must include `container.clusters.list`; update your controller role before using the `channel:` term.

---

### Image alias version pinning (`imageSelectorTerms[].alias`)

> **For new configurations, prefer the `family`/`channel`/`version` structured fields over the `alias` field.** See [Image selection](docs/image-selection.md) and [Image management](docs/image-management.md#legacy-image-alias-format) for the migration table.

**If you use `@latest` aliases:** Karpenter now reliably updates `GCENodeClass.status.images` whenever a new GKE node image is published. Combined with Karpenter's Drift mechanism, this means **all nodes using `@latest` will be replaced automatically when GKE releases a new image.** If you want to control when image updates roll out, pin to a specific version using the new structured fields:

```yaml
# ContainerOptimizedOS — milestone.build.build.build format
imageSelectorTerms:
  - family: ContainerOptimizedOS
    version: "125.19216.104.126"

# Ubuntu — vYYYYMMDD format
imageSelectorTerms:
  - family: Ubuntu2404
    version: "v20260416"
```

See [docs/image-management.md](docs/image-management.md) for commands to discover available versions.

**If you use pinned alias versions:** Invalid version formats (e.g. `ContainerOptimizedOS@125`) are now rejected at admission by the CRD webhook. Update any aliases that use partial or incorrectly formatted version strings before upgrading.

**If a pinned version does not exist in GCP:** The `GCENodeClass` `ImagesReady` condition now shows `False` with reason `ImageResolutionFailed` and a descriptive message within one minute, instead of failing silently.

### Bootstrap pool discovery (template pool elimination)

Karpenter no longer creates `karpenter-default`, `karpenter-ubuntu`, `karpenter-cos-arm64`, or `karpenter-ubuntu-arm64` node pools. Instead, it discovers an existing RUNNING cluster pool to read bootstrap metadata. The upgrade itself requires no action.

After confirming provisioning works correctly with the new version, delete the legacy pools at your own pace:

```bash
for pool in karpenter-ubuntu karpenter-cos-arm64 karpenter-ubuntu-arm64 karpenter-default; do
  gcloud container node-pools delete "$pool" \
    --cluster=CLUSTER_NAME \
    --location=CLUSTER_LOCATION \
    --quiet
done
```

The new last-resort fallback pool is named `karpenter-fallback` (not `karpenter-default`), so deleting the four legacy names is safe and unambiguous.

Rolling back to the previous version will re-create the legacy pools automatically.

See [Bootstrap pool selection](docs/bootstrap-pool.md) for configuration options and troubleshooting.

---

### Replace `roles/compute.admin` + `roles/container.admin` with a minimal custom role

**Action required for all existing installations.**

Previous installation instructions granted two broad predefined roles to the controller SA:

- `roles/compute.admin` — full write access to all Compute Engine resources in the project
- `roles/container.admin` — full admin access to all GKE resources in the project

These are far broader than required. Create a minimal custom role instead:

```sh
export PROJECT_ID=<your-project-id>
export GSA_NAME=karpenter-gsa   # the name of your Karpenter controller GCP service account

curl -fsSL https://raw.githubusercontent.com/cloudpilot-ai/karpenter-provider-gcp/main/deploy/iam/karpenter-controller-role.yaml \
    -o karpenter-controller-role.yaml
gcloud iam roles create karpenter_controller --project=$PROJECT_ID --file=karpenter-controller-role.yaml 2>/dev/null || \
gcloud iam roles update karpenter_controller --project=$PROJECT_ID --file=karpenter-controller-role.yaml

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

**Using Terraform?** The [`deploy/terraform/`](../deploy/terraform/) module handles all of the
above automatically — it creates the controller SA, the minimal custom role (sourced from
`deploy/iam/karpenter-controller-role.yaml`), and the project IAM binding in one apply:

```hcl
module "karpenter_iam" {
  source     = "./deploy/terraform"
  project_id = "<your-project-id>"
}
```

Run `terraform apply` and then remove the old broad bindings with the `gcloud` commands above.

### Scope `iam.serviceAccountUser` to the node SA (not project-wide)

**Action required if you followed the previous installation guide.**

The previous guide granted `roles/iam.serviceAccountUser` at project level (actAs any SA in the
project). This should be scoped to only the SA(s) Karpenter attaches to nodes. Add the scoped
binding before removing the broad one to avoid a permission gap:

```sh
# Add the SA-scoped binding first to avoid a permission gap.
# Use the Compute Engine default SA or your custom node SA — see install guide Step 1.
export NODE_SA_EMAIL=<your-node-sa>@<your-project-id>.iam.gserviceaccount.com
gcloud iam service-accounts add-iam-policy-binding $NODE_SA_EMAIL \
    --role roles/iam.serviceAccountUser \
    --member "serviceAccount:$GSA_NAME@$PROJECT_ID.iam.gserviceaccount.com" \
    --project $PROJECT_ID

# Remove the broad project-wide binding.
gcloud projects remove-iam-policy-binding $PROJECT_ID \
    --member="serviceAccount:$GSA_NAME@$PROJECT_ID.iam.gserviceaccount.com" \
    --role="roles/iam.serviceAccountUser"
```

If you override the node SA via `GCENodeClass.spec.serviceAccount` or `--node-pool-service-account`,
repeat the `add-iam-policy-binding` step for each SA you use.

### Use a dedicated minimal-privilege node service account (recommended)

**No immediate action required.** This section documents the recommended hardening step for new
and existing installations. Karpenter continues to work with the Compute Engine default SA if you
skip it.

By default, Karpenter attaches the Compute Engine default SA
(`<project-number>-compute@developer.gserviceaccount.com`) to provisioned nodes. This SA carries
broad `roles/editor`-equivalent permissions. Creating a dedicated node SA with only the
permissions GKE nodes actually need reduces blast radius if a node is compromised.

Create and configure the dedicated SA:

```sh
export PROJECT_ID=<your-project-id>
export GSA_NAME=karpenter-gsa          # your Karpenter controller SA name
export NODE_SA_NAME=karpenter-node

gcloud iam service-accounts create $NODE_SA_NAME --project=$PROJECT_ID
export NODE_SA_EMAIL=$NODE_SA_NAME@$PROJECT_ID.iam.gserviceaccount.com

# Minimal GKE node permissions (logging, monitoring, stackdriver metadata).
gcloud projects add-iam-policy-binding $PROJECT_ID \
    --member="serviceAccount:$NODE_SA_EMAIL" \
    --role="roles/container.nodeServiceAccount"

# If nodes pull images from Artifact Registry:
# gcloud projects add-iam-policy-binding $PROJECT_ID \
#     --member="serviceAccount:$NODE_SA_EMAIL" \
#     --role="roles/artifactregistry.reader"

# Grant iam.serviceAccountUser on the new SA so the controller can attach it to VMs.
# (If you already have iam.serviceAccountUser scoped to the Compute Engine default SA
# from the section above, keep that binding until you confirm the new SA works.)
gcloud iam service-accounts add-iam-policy-binding $NODE_SA_EMAIL \
    --role roles/iam.serviceAccountUser \
    --member "serviceAccount:$GSA_NAME@$PROJECT_ID.iam.gserviceaccount.com" \
    --project $PROJECT_ID
```

Tell Karpenter to use the new SA. Choose one of:

- **Cluster-wide default** — set via Helm or the `--default-nodepool-service-account` flag:

  ```sh
  helm upgrade karpenter ... \
    --set "controller.settings.defaultNodepoolServiceAccount=$NODE_SA_EMAIL"
  ```

- **Per-NodeClass** — set `spec.serviceAccount` on each `GCENodeClass`:

  ```yaml
  spec:
    serviceAccount: karpenter-node@<project-id>.iam.gserviceaccount.com
  ```

Once nodes are rolling with the new SA, optionally revoke `iam.serviceAccountUser` from the
Compute Engine default SA to close the broad binding:

```sh
gcloud iam service-accounts remove-iam-policy-binding \
    $(gcloud projects describe $PROJECT_ID --format='value(projectNumber)')-compute@developer.gserviceaccount.com \
    --role roles/iam.serviceAccountUser \
    --member "serviceAccount:$GSA_NAME@$PROJECT_ID.iam.gserviceaccount.com" \
    --project $PROJECT_ID
```

### Node service account

The service account resolution order is now:

1. `spec.serviceAccount` in GCENodeClass
2. `--default-nodepool-service-account` operator flag / `DEFAULT_NODEPOOL_SERVICE_ACCOUNT` env var
3. No explicit SA — GCE uses the project's Compute Engine default service account (`<project-number>-compute@developer.gserviceaccount.com`), the same default GKE applies when no SA is specified at node pool creation

The fallback to the template's service account list has been removed. If your NodeClass and operator flag are both unset, provisioned nodes will use the Compute Engine default SA. This matches GKE's own default, but GKE recommends using a dedicated SA with minimal permissions ([`roles/container.nodeServiceAccount`](https://cloud.google.com/kubernetes-engine/docs/how-to/hardening-your-cluster#use_least_privilege_sa)) for production clusters. Set `DEFAULT_NODEPOOL_SERVICE_ACCOUNT` or `spec.serviceAccount` accordingly.

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

#### Step 1 (optional) — rotate live nodes

Trigger a rolling replacement of your NodePools so that every replacement instance is
stamped with `goog-k8s-cluster-location`. After this step, any remaining instance that
still lacks the label is confirmed to have no live workload and is safe to treat as an
orphan.

```bash
kubectl annotate nodepool <NODEPOOL_NAME> "karpenter.k8s.gcp/force-rollout=$(date +%s)" --overwrite
```

Repeat for each NodePool. Wait for all nodes to finish replacing before proceeding.

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

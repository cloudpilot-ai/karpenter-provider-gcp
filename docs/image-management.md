# Managing GKE Node Images

Karpenter for GCP automatically selects the newest non-deprecated GKE node image compatible with your cluster's Kubernetes version. This page explains how image selection works, how to control it, and the trade-offs between stability and keeping up with security patches.

> **Warning:** Karpenter supports automatic image selection using the `@latest` version pin, but this is **not** recommended for production environments. When using `@latest`, a new GKE image release will cause Karpenter to drift all out-of-date nodes in the cluster, replacing them with nodes running the new image. We strongly recommend evaluating new images in a lower environment before rolling them out to production. More details on managing GKE node images can be found in this guide.

## How Karpenter Selects Images

Every `GCENodeClass` requires an `imageSelectorTerms` field. The simplest form uses an `alias` to let Karpenter pick the right image automatically:

```yaml
imageSelectorTerms:
  - alias: ContainerOptimizedOS@latest
```

Karpenter queries the `gke-node-images` GCP project (ContainerOptimizedOS) or `ubuntu-os-gke-cloud` (Ubuntu) for the newest non-deprecated image that matches your cluster's Kubernetes patch version. It derives arm64 and GPU variants from the same image name.

When a newer image is published — during a GKE node image patch or control-plane upgrade — Karpenter's Drift mechanism marks nodes using the old image as drifted. Those nodes are then replaced according to your configured disruption budgets.

## Image Alias Format

The `alias` field takes the form `Family@version`:

| Family                 | `@latest` behaviour                                      | Pinned version format         | Example                                  |
|------------------------|----------------------------------------------------------|-------------------------------|------------------------------------------|
| `ContainerOptimizedOS` | Newest COS GKE image for your K8s patch version          | `milestone.build.build.build` | `ContainerOptimizedOS@125.19216.104.126` |
| `Ubuntu`               | Newest Ubuntu 24.04 GKE image for your K8s minor version | `vYYYYMMDD`                   | `Ubuntu@v20260416`                       |

An invalid version format (for example `ContainerOptimizedOS@125`) is rejected at admission by the CRD webhook. If the pinned version does not exist in GCP, the `ImagesReady` condition on the `GCENodeClass` will be set to `False` with a descriptive message within one minute.

## Finding Available Versions

**ContainerOptimizedOS** — replace `1351` with your K8s version digits (e.g. 1.35.1 → 1351):

```bash
gcloud compute images list \
  --project=gke-node-images \
  --filter="name~'^gke-1351-.*-cos-[0-9].*-c-pre$' AND NOT deprecated:*" \
  --format="value(name)" \
  | sed 's/.*-cos-\([0-9][0-9]*-[0-9][0-9]*-[0-9][0-9]*-[0-9][0-9]*\)-c-pre/\1/' \
  | tr '-' '.' | sort -u
```

Sample output:

```
125.19216.104.126
125.19216.109.133
```

**Ubuntu** — replace `1-35` with your K8s minor version (e.g. 1.35.x → 1-35):

```bash
gcloud compute images list \
  --project=ubuntu-os-gke-cloud \
  --filter="name~'^ubuntu-gke-2404-1-35-amd64-v[0-9].*$' AND NOT deprecated:*" \
  --format="value(name)" \
  | sed 's/.*-\(v[0-9][0-9]*\)$/\1/' | sort -u
```

Sample output:

```
v20260401
v20260416
```

## Controlling Image Replacement

### Option 1: Pin to a specific version (recommended for production)

Nodes are only replaced when you explicitly update the alias version. This gives full control over when image changes roll out:

```yaml
imageSelectorTerms:
  - alias: ContainerOptimizedOS@125.19216.104.126
```

```yaml
imageSelectorTerms:
  - alias: Ubuntu@v20260416
```

**Trade-off:** You opt out of automatic security patches. You must manually update the version when critical CVEs are patched in new node images.

### Option 2: Pin to an exact image ID

Use the full GCP image resource path when you need to reference a specific image regardless of alias resolution:

```yaml
imageSelectorTerms:
  - id: projects/gke-node-images/global/images/gke-1351-gke1396004-cos-125-19216-104-126-c-pre
```

### Option 3: Use `@latest` with disruption budgets

Keep automatic image updates but pace the rollout to prevent cluster-wide churn:

```yaml
imageSelectorTerms:
  - alias: ContainerOptimizedOS@latest
```

In your `NodePool`:

```yaml
disruption:
  budgets:
    - nodes: 20%       # allow up to 20 % of nodes to be replaced simultaneously
    - nodes: "0"
      schedule: "0 0 * * mon-fri"   # no replacements during the business-hour freeze
      duration: 8h
      reasons:
        - Drifted
```

## Relationship to GKE Cluster Upgrades

Karpenter selects images scoped to your cluster's current Kubernetes version. When GKE upgrades your control plane, new images appear in the `@latest` feed. Pinned versions from before the upgrade continue to work until you update the pin — GKE does not forcibly remove them.

After a cluster upgrade, run the `gcloud compute images list` commands above (with the new K8s version prefix) to discover the available images for the upgraded version and update your pin accordingly.

> **Note:** Exact image IDs are tied to a specific Kubernetes version (the version is embedded in the image name, e.g. `gke-1351-...`). After a control-plane upgrade, nodes launched from an old `id:` selector will use an image built for the previous K8s version. Unlike alias-based pins, Karpenter's Drift mechanism will not detect this mismatch. Update or remove `id:` selectors after every control-plane upgrade.

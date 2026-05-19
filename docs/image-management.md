# Managing GKE Node Images

Karpenter automatically selects and tracks GKE node images based on your `imageSelectorTerms` configuration. This page covers how to find available image versions, control when image updates roll out, and manage the relationship between image selection and cluster upgrades.

For an overview of selection modes (channel tracking, version pin, raw image ID), see [Image selection](image-selection.md).

> **Warning:** Using `version: latest` or `channel:` terms means Karpenter will detect new image releases and mark existing nodes as drifted, triggering replacement according to your disruption budgets. We strongly recommend evaluating new images in a lower environment before rolling them out to production.

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

### Option 1: Channel tracking (recommended — always current)

Track a GKE release channel for automatic updates:

```yaml
imageSelectorTerms:
  - family: ContainerOptimizedOS
    channel: stable   # or: rapid, regular, extended, cluster
```

When GKE promotes a new build for the channel, Karpenter marks existing nodes as drifted and replaces them according to your disruption budgets.

### Option 2: Pin to a specific version (recommended for production stability)

Nodes are only replaced when you explicitly update the version. This gives full control over when image changes roll out:

```yaml
imageSelectorTerms:
  - family: ContainerOptimizedOS
    version: "125.19216.104.126"
```

```yaml
imageSelectorTerms:
  - family: Ubuntu2404
    version: "v20260416"
```

**Trade-off:** You opt out of automatic security patches. You must manually update the version when critical CVEs are patched in new node images.

### Option 3: Pin to an exact image ID

Use the full GCP image resource path when you need to reference a specific image regardless of family resolution:

```yaml
imageSelectorTerms:
  - id: projects/gke-node-images/global/images/gke-1351-gke1396004-cos-125-19216-104-126-c-pre
```

### Option 4: Use `version: latest` with disruption budgets

Keep automatic image updates but control when and how fast nodes are replaced. With `version: latest`, new nodes always receive the current image; existing nodes are replaced gradually as Karpenter's Drift mechanism marks them and disruption budgets permit replacement.

```yaml
imageSelectorTerms:
  - family: ContainerOptimizedOS
    version: latest
```

**Restrict drift replacement to a maintenance window** — nodes are replaced only during the scheduled window (here: Tue–Thu 15:00 UTC, 20 min); outside the window drift replacements are blocked. Consolidation (scale-down) and expiration still operate normally at all times.

```yaml
disruption:
  budgets:
    - nodes: "10"
      reasons:
        - Underutilized
        - Drifted
      schedule: "0 15 * * 2-4"
      duration: 20m
```

**Block drift replacement entirely** — only new nodes receive updated images; existing nodes are rotated gradually through consolidation and expiration events, never by explicit drift eviction.

```yaml
disruption:
  budgets:
    - nodes: "0"
      reasons:
        - Drifted
```

**Pace an always-on rollout** — allow continuous replacement but cap the blast radius:

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

Karpenter selects images scoped to your cluster's current Kubernetes version. When GKE upgrades your control plane, new images appear for the new version. Pinned versions from before the upgrade continue to work until you update the pin — GKE does not forcibly remove them.

After a cluster upgrade, run the `gcloud compute images list` commands above (with the new K8s version prefix) to discover available images for the upgraded version and update your pin accordingly.

> **Note:** Exact image IDs are tied to a specific Kubernetes version (the version is embedded in the image name, e.g. `gke-1351-...`). After a control-plane upgrade, nodes launched from an old `id:` selector will use an image built for the previous K8s version. Unlike channel or version-based selection, Karpenter's Drift mechanism will not detect this mismatch. Update or remove `id:` selectors after every control-plane upgrade.

## Legacy: Image Alias Format

The `alias` field (`Family@version`) is deprecated. Use `family` with `channel` or `version` for new configurations.

| Old alias syntax                                | Replacement                                                    |
|-------------------------------------------------|----------------------------------------------------------------|
| `alias: ContainerOptimizedOS@latest`            | `family: ContainerOptimizedOS`, `version: latest`              |
| `alias: Ubuntu@latest`                          | `family: Ubuntu2404`, `version: latest`                        |
| `alias: ContainerOptimizedOS@125.19216.104.126` | `family: ContainerOptimizedOS`, `version: "125.19216.104.126"` |
| `alias: Ubuntu@v20260416`                       | `family: Ubuntu2404`, `version: "v20260416"`                   |

The `alias` field continues to work and is validated at admission. It will be removed in a future release.

# Managing GKE Node Images

Karpenter automatically selects and tracks GKE node images based on your `imageSelectorTerms` configuration. This page covers how to find available image versions, control when image updates roll out, and manage the relationship between image selection and cluster upgrades.

For an overview of selection modes (channel tracking, version pin, raw image ID), see [Image selection](image-selection.md).

> **Warning:** Using `version: latest` or `channel:` terms means Karpenter will detect new image releases and mark existing nodes as drifted, triggering replacement according to your disruption budgets. We strongly recommend evaluating new images in a lower environment before rolling them out to production.

## Finding Available Versions

Karpenter only selects images whose GCE status is `READY`. The commands below apply the same filter, so an image that GCP is still publishing doesn't appear until you can use it.

**ContainerOptimizedOS** — replace `1351` with your K8s version digits (e.g. 1.35.1 → 1351):

```bash
gcloud compute images list \
  --project=gke-node-images \
  --filter="name~'^gke-1351-.*-cos-[0-9].*-c-pre$' AND NOT deprecated:* AND status=READY" \
  --format="value(name)" \
  | sed 's/.*-cos-\([0-9][0-9]*-[0-9][0-9]*-[0-9][0-9]*-[0-9][0-9]*\)-c-pre/\1/' \
  | tr '-' '.' | sort -u
```

Sample output:

```
125.19216.104.126
125.19216.109.133
```

The filter above lists the amd64 CPU variant only. arm64 CPU images are named `...-cos-arm64-<version>-c-pre` and NVIDIA GPU images `...-cos-<version>-c-nvda`. `version: latest` ranks all three variants separately, so a new node can receive a different date per variant: the newest amd64 date above is not selected while it is still `PENDING`, and an older `READY` date is used instead. To list every variant, drop the `-c-pre` suffix from the filter and keep the `-c-pre`/`-c-nvda` suffixes in the output:

```bash
gcloud compute images list \
  --project=gke-node-images \
  --filter="name~'^gke-1351-gke[0-9]+-cos-.*-(c-pre|c-nvda)$' AND NOT deprecated:* AND status=READY" \
  --format="value(name)" \
  | sort
```

**Ubuntu** — replace `1-35` with your K8s minor version (e.g. 1.35.x → 1-35):

```bash
gcloud compute images list \
  --project=ubuntu-os-gke-cloud \
  --filter="name~'^ubuntu-gke-2404-1-35-amd64-v[0-9]+[a-z]?$' AND NOT deprecated:* AND status=READY" \
  --format="value(name)" \
  | grep -oE 'v[0-9]+[a-z]?$' | sort -u
```

Sample output:

```
v20260401
v20260416a
```

For arm64, change the arch token in the filter (`amd64` → `arm64`):

```bash
gcloud compute images list \
  --project=ubuntu-os-gke-cloud \
  --filter="name~'^ubuntu-gke-2404-1-35-arm64-v[0-9]+[a-z]?$' AND NOT deprecated:* AND status=READY" \
  --format="value(name)" \
  | grep -oE 'v[0-9]+[a-z]?$' | sort -u
```

Sample output:

```
v20260408
v20260420a
```

For Ubuntu 22.04, the `ubuntu-gke-2204` amd64 image names do not carry an `-amd64-` token. Drop the arch token from the amd64 filter: use `^ubuntu-gke-2204-1-35-v[0-9]+[a-z]?$` for amd64 and `^ubuntu-gke-2204-1-35-arm64-v[0-9]+[a-z]?$` for arm64.

> **Note:** A date shown with a trailing letter (for example, `v20260416a`) is a re-spin that `version: latest` can select, but it is not a valid `version:` pin — pins must be a bare `vYYYYMMDD`. Pin only dates that appear without a trailing letter.

> **Note:** For Ubuntu families, Karpenter resolves both architectures independently from the catalog, so both an amd64 image and an arm64 image must exist for the cluster's Kubernetes minor version before any NodePool referencing the GCENodeClass can become ready. If either image is missing, the GCENodeClass's `ImagesReady` condition reports `ImageResolutionFailed`. See [Troubleshooting: GCENodeClass not becoming Ready](troubleshooting.md#gcenodeclass-not-becoming-ready).

## Controlling Image Replacement

### Option 1: Channel tracking (recommended)

Track a GKE release channel for automatic updates:

```yaml
imageSelectorTerms:
  - family: ContainerOptimizedOS
    channel: stable   # or: rapid, regular, extended, cluster
```

When GKE promotes a new build for the channel, Karpenter marks existing nodes as drifted and replaces them according to your disruption budgets.

### Option 2: Pin to a specific version

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

For an overview of how Karpenter ranks images when GKE publishes variants at different times, and for the `ImagesReady` condition reasons, see [Image selection: Container-Optimized OS variants](image-selection.md#container-optimized-os-variants).

## How image refreshes and failures are handled

Karpenter resolves a NodeClass's image selector terms and publishes the usable images in `status.images`. A successful resolution is cached per selector for up to five minutes, so a newly published image is picked up on the next refresh rather than immediately, and an image GCP deletes or deprecates just after publication can still be selected for a few minutes. Failures and empty results are never cached, because the next refresh may succeed.

`ImagesReady` reports what Karpenter can currently publish:

| Reason                  | Meaning                                                                                                                                                                                                                                                                                       | Retry                    |
|-------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------|
| *(condition `True`)*    | At least one usable image is published. For Container-Optimized OS this can be a partial set of the amd64 CPU, arm64 CPU, and NVIDIA variants.                                                                                                                                                | Every 5 minutes          |
| `ImageResolutionFailed` | The selector cannot resolve. Either it is a configuration error, for example an unsupported version format or an unsupported family, or an Ubuntu architecture has no usable image in the catalog yet. The Ubuntu case needs no configuration change: it clears once GCP publishes the image. | Every 1 minute           |
| `ImageUnavailable`      | The selector is valid but GCP has no usable image published yet, for example a `latest` term whose newest candidates are still `PENDING`, an exact channel build or milestone pin whose COS image is not published yet, or a raw image ID that is not `READY`. `status.images` is cleared.    | Every 1 minute           |
| `ImageLookupFailed`     | A Compute API call failed (permissions, rate limits, or a transient server error). Any images left in `status.images` are from the previous successful resolution and are diagnostic only.                                                                                                    | Controller error backoff |
| `ImagesNotFound`        | The selector resolved but matched nothing, for example a raw image ID that does not exist yet. `status.images` is cleared.                                                                                                                                                                    | Every 1 minute           |

While `ImagesReady` is not `True`, Karpenter exposes no instance types for the NodeClass and starts no new nodes from the images left in status. Existing nodes keep running and are only replaced once a published image is compatible with them.

For Ubuntu both architectures must resolve from the catalog: if either has no usable image, including one that is still `PENDING`, the term fails and the NodeClass does not become ready from a single architecture alone. The final per-image lookup that validates a published set can still drop an architecture whose image has disappeared by then, so a published set is not a guarantee that every image still exists a moment later.

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

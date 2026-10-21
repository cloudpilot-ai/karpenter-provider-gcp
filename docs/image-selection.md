# Image Selection

Karpenter selects node images using the `imageSelectorTerms` field in GCENodeClass. You can specify images using OS family and GKE release channel, pin to a specific version, or reference a raw image ID.

## Quick start

Follow your cluster's enrolled GKE release channel:

```yaml
apiVersion: karpenter.k8s.gcp/v1alpha1
kind: GCENodeClass
metadata:
  name: default
spec:
  imageSelectorTerms:
    - family: ContainerOptimizedOS
      channel: cluster
```

## Selection modes

Each term in `imageSelectorTerms` must use exactly one of three selection modes:

| Mode              | Fields               | Behavior                                                                                        |
|-------------------|----------------------|-------------------------------------------------------------------------------------------------|
| Channel reference | `family` + `channel` | Resolves to the GKE build promoted for that channel. Nodes drift when GKE promotes a new build. |
| Version pin       | `family` + `version` | Fixed image. Nodes never drift from image changes.                                              |
| Raw image ID      | `id`                 | Explicit GCE image URL. Nodes never drift from image changes.                                   |

Karpenter does not start a node from an image GCP has not finished publishing: a `READY` status is required, both for family selectors (`family`/`alias`) and for a raw `id`. Family selectors also exclude `DEPRECATED`, `OBSOLETE`, and `DELETED` images even though GCP still allows starting an instance from a deprecated image; a raw `id` checks the image status only, not its deprecation state. Container-Optimized OS resolves several variants independently; see [Container-Optimized OS variants](#container-optimized-os-variants).

### Channel reference (live)

Track a GKE release channel for Container-Optimized OS:

```yaml
imageSelectorTerms:
  - family: ContainerOptimizedOS
    channel: stable
```

When GKE promotes a new build for the channel, Karpenter marks existing nodes as drifted and replaces them according to your disruption budgets.

Available channels:

| Channel    | Description                                         |
|------------|-----------------------------------------------------|
| `rapid`    | Newest builds, least validation                     |
| `regular`  | Balance of new features and stability               |
| `stable`   | Most validated, recommended for production          |
| `extended` | Extended support for older Kubernetes versions      |
| `cluster`  | Follows whatever channel the cluster is enrolled in |

Use `channel: cluster` as the default for most deployments. It automatically tracks the cluster's enrolled channel without hardcoding a specific one.

Channel resolution is exact: Karpenter looks up the COS images for the GKE build promoted for the channel and never substitutes another build. The arm64 and NVIDIA siblings of that build are still checked individually, so a sibling GCP has not published is omitted (the published variants remain) instead of being replaced by another build's image. If the promoted build has no usable amd64 image yet, `ImagesReady=False` with reason `ImageUnavailable` is reported and resolution is retried after one minute.

Channel selection is only supported for Container-Optimized OS. Ubuntu images do not have per-channel differentiation in GKE.

### Version pin (static)

Pin to a specific image version when you need deterministic builds:

```yaml
# Container-Optimized OS pinned to a specific COS milestone
imageSelectorTerms:
  - family: ContainerOptimizedOS
    version: "125.19216.104.126"

# Ubuntu 24.04 pinned to a specific date build
imageSelectorTerms:
  - family: Ubuntu2404
    version: "v20260416"
```

A Container-Optimized OS milestone pin keeps its exact milestone. Karpenter matches the pin to a build for the cluster's Kubernetes patch version to construct the variant image names, then publishes only the pinned variants that exist and are `READY`; it never falls back to a different milestone. If GCP has not published the pinned images yet, `ImagesReady=False` with reason `ImageUnavailable` is reported and resolution is retried automatically.

Use `version: latest` to select the newest image for the cluster's Kubernetes version without channel awareness:

```yaml
imageSelectorTerms:
  - family: Ubuntu2404
    version: latest
```

### Raw image ID

Reference a specific GCE image by its full resource URL:

```yaml
imageSelectorTerms:
  - id: projects/gke-node-images/global/images/gke-1351-gke1396004-cos-125-19216-104-126-c-pre
```

Raw image IDs are strict: the referenced image must exist and be `READY`, and nothing is substituted for it. An image that does not exist yet leaves the term with no images, so `status.images` is cleared and `ImagesReady=False` with reason `ImagesNotFound` is reported; GCP publishing it later resolves the term.

## Container-Optimized OS variants

GKE publishes Container-Optimized OS as three variants: an amd64 CPU image (`-c-pre`), an arm64 CPU image (`-cos-arm64-...-c-pre`), and an amd64 NVIDIA GPU image (`-c-nvda`). Karpenter resolves each variant independently and publishes only the variants it can use, so `ImagesReady=True` means at least one usable variant exists, not that every architecture or GPU shape can start a node.

Specialized builds published in the same project are never selected, including kmod, gVisor, `-test`, and cgroup v1 (`cgpv1`) images.

### Ranked fallback for `version: latest`

GKE publishes the variants at different times, so `version: latest` is not a single image: Karpenter ranks the COS catalog newest first and takes the newest usable image for each variant. If the newest amd64 image is still `PENDING`, an older `READY`, non-deprecated amd64 image for the same Kubernetes patch version is used instead (when the cluster's Kubernetes version cannot be read, the scope widens to all COS images). The fallback is not limited to one step back, and nothing is remembered between refreshes: a variant moves to a newer image as soon as GCP publishes one, and an image that disappears or becomes deprecated stops being selected on the next refresh.

## Partial image sets

A partial Container-Optimized OS image set is published on purpose, so a variant GKE has not published yet does not block the nodes the published variants can run. With only an amd64 CPU image published:

- **Scheduling** offers only instance types that a published image can boot. arm64 and GPU instance types are not offered, so an arm64-only NodePool stays pending until GCP publishes an arm64 image.
- **New launches** use only published images. A NodeClaim that no published image can back fails with an insufficient-capacity error naming the image problem; zonal capacity is not marked unavailable.
- **Existing nodes** keep running. Image drift is reported only when a published image is compatible with the node's actual architecture and GPU, so retained arm64 and GPU nodes are not replaced toward an image they cannot boot.

When no usable image can be published for a valid selector, `ImagesReady=False` with reason `ImageUnavailable`, the previous `status.images` is cleared, and nothing is scheduled or launched. See [Troubleshooting: GCENodeClass not becoming Ready](troubleshooting.md#gcenodeclass-not-becoming-ready) for what each condition reason means.

A successful resolution is refreshed at most every five minutes, and failures are retried sooner: one minute for publication or configuration problems, and the controller's normal error backoff for Compute API failures. A newly published image therefore appears within about five minutes, and an image GCP deletes or deprecates immediately after publication can still be used for a few minutes.

## OS families

| Family                 | OS                     | Notes                            |
|------------------------|------------------------|----------------------------------|
| `ContainerOptimizedOS` | Container-Optimized OS | Supports `channel` and `version` |
| `Ubuntu2404`           | Ubuntu 24.04 LTS       | Supports `version` only          |
| `Ubuntu2204`           | Ubuntu 22.04 LTS       | Supports `version` only          |

The bare `Ubuntu` family name is not supported. Use `Ubuntu2404` or `Ubuntu2204` explicitly.

> **Note:** Ubuntu images are resolved per architecture, independently, from the `ubuntu-os-gke-cloud` catalog for the cluster's Kubernetes minor version. Only standard date builds are selected — `vYYYYMMDD`, optionally with a trailing letter (for example, `v20260416a` may be resolved by `version: latest`). Specialized GKE variant builds are excluded from selection, such as `-tpu`, `-cgroupsv1`, or `-linux64k`. Both architectures must resolve from that catalog before anything is published, and only `READY` images are eligible there, so if either architecture has no usable image for the cluster's Kubernetes minor version, resolution fails and the GCENodeClass's `ImagesReady` condition reports `ImageResolutionFailed` — including when the missing image is still `PENDING` in the catalog. `ImageUnavailable` is reported only when an image listed as `READY` is no longer ready when Karpenter looks it up, and it resolves once GCP finishes publishing. That final lookup runs per architecture, so an image that has disappeared by then is dropped from the published set rather than reported as an error. Use [Finding Available Versions](image-management.md#finding-available-versions) to confirm a date is published for both amd64 and arm64 before pinning, regardless of the NodePool's own architecture. amd64 and arm64 builds for a given date are published independently, so a `version: vYYYYMMDD` pin available for one architecture may not exist for the other. Pinning a date present for only one of the two architectures fails the same way, rather than silently synthesizing a nonexistent image.

## Version formats

| Family                 | `version: latest`                           | Version pin format            | Example             |
|------------------------|---------------------------------------------|-------------------------------|---------------------|
| `ContainerOptimizedOS` | Newest usable COS image per variant         | `milestone.build.build.build` | `125.19216.104.126` |
| `Ubuntu2404`           | Newest Ubuntu 24.04 for cluster's K8s minor | `vYYYYMMDD`                   | `v20260416`         |
| `Ubuntu2204`           | Newest Ubuntu 22.04 for cluster's K8s minor | `vYYYYMMDD`                   | `v20231201`         |

## Examples

### Production COS with channel tracking

```yaml
apiVersion: karpenter.k8s.gcp/v1alpha1
kind: GCENodeClass
metadata:
  name: cos-stable
spec:
  imageSelectorTerms:
    - family: ContainerOptimizedOS
      channel: stable
```

### Ubuntu 24.04 with automatic updates

```yaml
apiVersion: karpenter.k8s.gcp/v1alpha1
kind: GCENodeClass
metadata:
  name: ubuntu
spec:
  imageSelectorTerms:
    - family: Ubuntu2404
      version: latest
```

### Pinned COS version for compliance

```yaml
apiVersion: karpenter.k8s.gcp/v1alpha1
kind: GCENodeClass
metadata:
  name: cos-pinned
spec:
  imageSelectorTerms:
    - family: ContainerOptimizedOS
      version: "125.19216.104.126"
```

## Drift and disruption

When using `channel:` terms, nodes are marked as drifted when GKE promotes a new build for the channel. Karpenter replaces drifted nodes according to your NodePool's disruption budgets.

Image drift is evaluated per variant. Advancing the amd64 image does not drift an arm64 or GPU node, and a node is only marked as image-drifted once a published image can replace the one it actually runs. While resolution is failing, or while no published image is compatible with the node, image drift is suppressed instead of evicting a node that could not be replaced. Configuration drift (for example a changed GCENodeClass field) is still reported.

To control replacement timing, configure `spec.disruption` on your NodePool:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
spec:
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    budgets:
      - nodes: "10%"
```

## Migration from alias

The `alias` field is deprecated. Migrate to the new structured fields:

| Old syntax                                      | New syntax                                                       |
|-------------------------------------------------|------------------------------------------------------------------|
| `alias: ContainerOptimizedOS@latest`            | `family: ContainerOptimizedOS`<br>`version: latest`              |
| `alias: Ubuntu@latest`                          | `family: Ubuntu2404`<br>`version: latest`                        |
| `alias: ContainerOptimizedOS@125.19216.104.126` | `family: ContainerOptimizedOS`<br>`version: "125.19216.104.126"` |

The `alias` field continues to work but will be removed in a future release. New configurations should use `family` with `channel` or `version`.

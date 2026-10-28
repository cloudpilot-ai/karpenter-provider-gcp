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

Images must be `READY` before use. Family discovery excludes `DEPRECATED`, `OBSOLETE`, and `DELETED` images; raw `id` selectors check status but not deprecation.

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

Channel resolution never substitutes another GKE build. If the promoted build has no usable amd64 base image yet, resolution retries with `ImagesReady=False` / `ImageUnavailable`.

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

## Container-Optimized OS variants

Karpenter supports independently published amd64 CPU (`-c-pre`), arm64 CPU (`-cos-arm64-...-c-pre`), and amd64 NVIDIA (`-c-nvda`) images. Specialized builds such as kmod, gVisor, `-test`, and cgroup v1 are excluded.

With `version: latest` (including the legacy alias), Karpenter selects the newest existing, `READY`, non-deprecated image **per variant** for the cluster's Kubernetes patch version. Unusable newer candidates are skipped until an eligible image is found; there is no one-build fallback limit or last-known-good history. If the cluster version cannot be read, discovery uses the existing broader COS scope. Channels and milestone pins keep their exact target, omitting unavailable siblings rather than substituting another build or version.

`ImagesReady=True` can therefore contain a partial set. Only instance types backed by a resolved image offer new scheduling or launch capacity; unsupported requests wait or fail with an image-related capacity error, without poisoning the zonal capacity cache. Unsupported types and their offering requirements remain visible to running-node drift checks, with their scheduling offerings marked unavailable. Existing VMs remain adoptable on retries, even while image readiness is false or unknown; an unready class still cannot launch a new VM. Image drift waits for a compatible replacement; see [Drift and disruption](#drift-and-disruption).

Successful results are cached for five minutes, so newly published variants and deleted/deprecated images are noticed on refresh, not instantly. Failures and empty results are not cached. While `ImagesReady` is not `True`, retained image status cannot authorize new launches. See [image readiness troubleshooting](troubleshooting.md#gcenodeclass-not-becoming-ready) for conditions, status clearing and retry intervals.

## OS families

| Family                 | OS                     | Notes                            |
|------------------------|------------------------|----------------------------------|
| `ContainerOptimizedOS` | Container-Optimized OS | Supports `channel` and `version` |
| `Ubuntu2404`           | Ubuntu 24.04 LTS       | Supports `version` only          |
| `Ubuntu2204`           | Ubuntu 22.04 LTS       | Supports `version` only          |

The bare `Ubuntu` family name is not supported. Use `Ubuntu2404` or `Ubuntu2204` explicitly.

> **Ubuntu:** Both architectures must resolve from `ubuntu-os-gke-cloud` for the cluster's Kubernetes minor version. Only clean `vYYYYMMDD` builds, optionally with a trailing letter (for example, `v20260416a`), are selected, excluding specialized builds such as `-tpu`, `-cgroupsv1`, and `-linux64k`. A pin matches one build exactly, so pin a lettered build with its letter (`version: v20260416a`). A missing or `PENDING` catalog architecture yields `ImageResolutionFailed`; a selected image becoming non-READY on final lookup yields `ImageUnavailable`. Final-lookup 404s are omitted. Before pinning a date, [confirm it exists for both architectures](image-management.md#finding-available-versions).

## Version formats

| Family                 | `version: latest`                           | Version pin format                    | Example                   |
|------------------------|---------------------------------------------|---------------------------------------|---------------------------|
| `ContainerOptimizedOS` | Newest usable COS image per variant         | `milestone.build.build.build`         | `125.19216.104.126`       |
| `Ubuntu2404`           | Newest Ubuntu 24.04 for cluster's K8s minor | `vYYYYMMDD`, optional trailing letter | `v20260416`, `v20251218a` |
| `Ubuntu2204`           | Newest Ubuntu 22.04 for cluster's K8s minor | `vYYYYMMDD`, optional trailing letter | `v20231201`, `v20251218a` |

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

Image drift requires ready resolution and a replacement compatible with the node's actual architecture/GPU variant. An amd64 update alone does not drift arm64 or GPU nodes. This guard does not suppress configuration drift.

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

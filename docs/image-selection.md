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
  - family: Ubuntu
    osVersion: "24.04"
    version: "v20260416"
```

Use `version: latest` to select the newest image for the cluster's Kubernetes version without channel awareness:

```yaml
imageSelectorTerms:
  - family: Ubuntu
    osVersion: "24.04"
    version: latest
```

### Raw image ID

Reference a specific GCE image by its full resource URL:

```yaml
imageSelectorTerms:
  - id: projects/gke-node-images/global/images/gke-1351-gke1396004-cos-125-19216-104-126-c-pre
```

## Bootstrap release

Resolved `status.images[]` rows include `kubernetesVersion`, the core GKE release to install with that image. The controller keeps image and release together through caching and launch; it does not reread a new master version after selecting an older cached image.

- **Official COS images:** use the selected image's GKE release, not necessarily the master's exact patch/build. This preserves channel, milestone and explicit image pins. Modern official names with a two-digit Kubernetes minor are decoded, including multi-digit patches; ambiguous historical formats are not guessed.
- **Ubuntu:** use one exact API-server version snapshot for both minor-scoped image discovery and bootstrap. An Ubuntu date pin fixes the OS image, not the kubelet release. Image discovery uses the Ubuntu image project that serves that minor (see [OS families](#os-families)).
- **Opaque custom image IDs:** keep the pinned image/architecture and use an exact API-server version snapshot. A custom-project image is not treated as official COS merely because its name resembles one. Custom images must be compatible with the chosen bootstrap release.

Core server/manifests URLs and hashes, `KUBELET_VERSION`, `configure-sh` and `user-data` come from the selected release's public GKE distribution, not the source pool. Legacy SALT fields and the competing source startup script are not inherited. Other source runtime policy, including `RENDERED_INSTALLABLES`, remains unchanged under the accepted compatibility assumption; see [Bootstrap pool selection](bootstrap-pool.md#core-bootstrap-release-and-architecture).

The controller needs HTTPS access to `https://storage.googleapis.com/gke-release/kubernetes/release/` for small scripts and checksum sidecars. It caches complete verified bundles with a bounded cache. Nodes use matching preloads or fetch the corresponding archives; an artifact lookup/integrity failure stops a new launch rather than falling back to source artifacts or marking capacity unavailable. No new mirror/alternate-universe endpoint setting is introduced.

Upgrade CRDs before the controller, then allow normal image-status refresh. Old CRDs can prune the new field; a new launch with an incomplete row waits for reconciliation. Existing VMs remain adoptable without a current artifact lookup. A release-only change with the same image does not introduce drift or replace running nodes.

## Container-Optimized OS variants

Karpenter supports independently published amd64 CPU (`-c-pre`), arm64 CPU (`-cos-arm64-...-c-pre`), and amd64 NVIDIA (`-c-nvda`) images. Specialized builds such as kmod, gVisor, `-test`, and cgroup v1 are excluded.

With `version: latest` (including the legacy alias), Karpenter selects the newest existing, `READY`, non-deprecated image **per variant** for the cluster's Kubernetes patch version. Unusable newer candidates are skipped until an eligible image is found; there is no one-build fallback limit or last-known-good history. If the cluster version cannot be read, discovery uses the existing broader COS scope. Channels and milestone pins keep their exact target, omitting unavailable siblings rather than substituting another build or version.

`ImagesReady=True` can therefore contain a partial set. Only instance types backed by a resolved image offer new scheduling or launch capacity; unsupported requests wait or fail with an image-related capacity error, without poisoning the zonal capacity cache. Unsupported types and their offering requirements remain visible to running-node drift checks, with their scheduling offerings marked unavailable. Existing VMs remain adoptable on retries, even while image readiness is false or unknown; an unready class still cannot launch a new VM. Image drift waits for a compatible replacement; see [Drift and disruption](#drift-and-disruption).

Successful results are cached for five minutes, so newly published variants and deleted/deprecated images are noticed on refresh, not instantly. Failures and empty results are not cached. While `ImagesReady` is not `True`, retained image status cannot authorize new launches. See [image readiness troubleshooting](troubleshooting.md#gcenodeclass-not-becoming-ready) for conditions, status clearing and retry intervals.

## OS families

| Family                 | OS                     | Notes                                                                     |
|------------------------|------------------------|---------------------------------------------------------------------------|
| `ContainerOptimizedOS` | Container-Optimized OS | Supports `channel` and `version`                                          |
| `Ubuntu`               | Ubuntu                 | Requires `osVersion` (`22.04`, `24.04`, `26.04`); supports `version` only |
| `Ubuntu2404`           | Ubuntu 24.04 LTS       | **Deprecated.** Same as `Ubuntu` with `osVersion: "24.04"`                |
| `Ubuntu2204`           | Ubuntu 22.04 LTS       | **Deprecated.** Same as `Ubuntu` with `osVersion: "22.04"`                |

`osVersion` is required for `Ubuntu` and rejected for other families, so Karpenter never picks an Ubuntu release for you and never moves nodes to a new release on its own. Changing `osVersion` is an explicit image change: nodes drift and are replaced under your disruption budgets. The format is `NN.04`; a release Karpenter does not know yet fails image resolution with the supported releases listed, and adding one needs no CRD change. `version` still pins the build within that release. The deprecated `Ubuntu2404` and `Ubuntu2204` families and the `Ubuntu@...` alias (always Ubuntu 24.04) keep working and resolve identically; switching a NodeClass between the spellings does not drift nodes.

> **Ubuntu:** Both architectures must resolve for the cluster's Kubernetes minor version from the project GKE publishes Ubuntu images in: `ubuntu-os-gke-cloud` for Kubernetes before 1.36 and `gke-ubuntu-node-images` for 1.36 and later. The controller needs permission to read that project's images, and a trusted-image-project policy or VPC Service Controls perimeter must allow it. The two projects are never mixed or used as a fallback for each other. Only clean `vYYYYMMDD` builds, optionally with a trailing letter (for example, `v20260416a`), are selected, excluding specialized builds such as `-tpu`, `-cgroupsv1`, `-linux64k`, `-64k`, `-hwe`, and `-acs`. A pin matches one build exactly, so pin a lettered build with its letter (`version: v20260416a`). A missing or `PENDING` catalog architecture yields `ImageResolutionFailed`; a selected image becoming non-READY on final lookup yields `ImageUnavailable`. Final-lookup 404s are omitted. Before pinning a date, [confirm it exists for both architectures](image-management.md#finding-available-versions).
>
> **Release availability:** GKE publishes each Ubuntu release only for some Kubernetes minors, and a release is never substituted for another. As of November 2026, Ubuntu 22.04 images exist only through Kubernetes 1.31, Ubuntu 24.04 from 1.31 through 1.37, and Ubuntu 26.04 for 1.37 and 1.38 (1.38 is an alpha-only minor), so `osVersion: "24.04"` finds no image on a 1.38 cluster. In that case `ImageResolutionFailed` names the release and project; move the NodeClass to the series that has images for your cluster version.

## Version formats

| Family                   | `version: latest`                                    | Version pin format                    | Example                   |
|--------------------------|------------------------------------------------------|---------------------------------------|---------------------------|
| `ContainerOptimizedOS`   | Newest usable COS image per variant                  | `milestone.build.build.build`         | `125.19216.104.126`       |
| `Ubuntu` (+ `osVersion`) | Newest image of that release for cluster's K8s minor | `vYYYYMMDD`, optional trailing letter | `v20260416`, `v20251218a` |

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
    - family: Ubuntu
      osVersion: "24.04"
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
| `alias: Ubuntu@latest`                          | `family: Ubuntu`<br>`osVersion: "24.04"`<br>`version: latest`    |
| `alias: ContainerOptimizedOS@125.19216.104.126` | `family: ContainerOptimizedOS`<br>`version: "125.19216.104.126"` |

The `alias` field continues to work but will be removed in a future release. New configurations should use `family` with `channel` or `version`.

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

## OS families

| Family                 | OS                     | Notes                            |
|------------------------|------------------------|----------------------------------|
| `ContainerOptimizedOS` | Container-Optimized OS | Supports `channel` and `version` |
| `Ubuntu2404`           | Ubuntu 24.04 LTS       | Supports `version` only          |
| `Ubuntu2204`           | Ubuntu 22.04 LTS       | Supports `version` only          |

The bare `Ubuntu` family name is not supported. Use `Ubuntu2404` or `Ubuntu2204` explicitly.

## Version formats

| Family                 | `version: latest`                           | Version pin format            | Example             |
|------------------------|---------------------------------------------|-------------------------------|---------------------|
| `ContainerOptimizedOS` | Newest COS for cluster's K8s version        | `milestone.build.build.build` | `125.19216.104.126` |
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

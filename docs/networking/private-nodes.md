# Private Nodes

This page explains how to control external IP assignment and subnetwork placement for nodes provisioned by Karpenter.

## How Karpenter discovers the bootstrap source pool

When Karpenter starts, it selects an existing RUNNING GKE node pool to use as the bootstrap metadata source (kubelet configuration, instance template, network settings). The selection follows this priority order:

1. The pool named by the `DEFAULT_NODEPOOL_TEMPLATE_NAME` environment variable (if set and RUNNING)
2. The pool named `default-pool` (if RUNNING)
3. The first RUNNING pool in alphabetical order

If no RUNNING pool is found, Karpenter creates a minimal fallback pool named `karpenter-default` (COS, amd64) and waits for it to reach RUNNING state before proceeding. Unlike the old model, Karpenter does **not** create pools proactively at startup.

To pin the bootstrap source pool explicitly — for example on a cluster where org policies restrict pool creation — set `DEFAULT_NODEPOOL_TEMPLATE_NAME` in the Helm values:

```yaml
controller:
  settings:
    defaultNodePoolTemplateName: "my-existing-pool"
```

## Cluster-level private nodes (untested)

Karpenter GCP has not been tested on clusters that enforce private nodes at the cluster level (e.g. `DefaultEnablePrivateNodes: true` or `PrivateClusterConfig.enablePrivateNodes: true`). It may not work.

On such clusters, the fallback pool creation (`karpenter-default`) may be rejected by GKE because `NodePool.NetworkConfig` is not set to enforce private nodes. To avoid this, pin the bootstrap source to an existing RUNNING pool using `DEFAULT_NODEPOOL_TEMPLATE_NAME` — Karpenter will then discover that pool instead of creating a new one.

The `enableExternalIPAccess: false` field on `GCENodeClass` controls the external IP of **provisioned instances** but does not affect pool discovery or fallback pool creation.

Support for cluster-level private nodes is tracked in [GitHub issue #230](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/230).

## Selectively disabling external IPs via NodeClass

On a standard GKE cluster (public nodes), you may want Karpenter to provision nodes without external IPs while leaving other node pools public. Use `networkConfig.networkInterfaces` on a `GCENodeClass` to override the access config that Karpenter inherits from its template pool.

```yaml
networkConfig:
  networkInterfaces:
    - enableExternalIPAccess: false
```

Setting `enableExternalIPAccess: false` removes the `ONE_TO_ONE_NAT` access config from the primary interface of every node Karpenter provisions via that `GCENodeClass`. The template pool itself (`karpenter-default`) is not modified.

### Prerequisites

1. **Cloud NAT** — nodes without an external IP have no outbound internet access unless Cloud NAT is configured on the VPC. Without it, nodes cannot pull container images from public registries or reach GCP APIs. See: [Using Cloud NAT with GKE](https://cloud.google.com/nat/docs/gke-example)

2. **VPC-native cluster** — the cluster must use alias IP ranges (VPC-native mode). This is the default for all new GKE clusters. See: [VPC-native clusters](https://cloud.google.com/kubernetes-engine/docs/concepts/alias-ips)

3. **Node access** — without an external IP, direct SSH and `kubectl exec` require [Identity-Aware Proxy (IAP)](https://cloud.google.com/iap/docs/using-tcp-forwarding) or a bastion host.

### Example

See [NodePool examples — Private nodes](../examples/networking.md#private-nodes-no-external-ip).

## Overriding the subnetwork

By default, nodes are placed in the subnetwork that the `karpenter-default` template inherits from the cluster. Use `networkConfig.networkInterfaces[].subnetwork` to place Karpenter nodes in a different subnetwork — for example one with a narrower CIDR or separate firewall rules.

```yaml
networkConfig:
  networkInterfaces:
    - subnetwork: regions/us-central1/subnetworks/karpenter-nodes
```

The value must be a [self-link or partial URL](https://cloud.google.com/compute/docs/reference/rest/v1/instances#networkinterface):

- Partial: `regions/REGION/subnetworks/NAME`
- Full: `https://www.googleapis.com/compute/v1/projects/PROJECT/regions/REGION/subnetworks/NAME`

### Example

See [NodePool examples — Custom subnetwork](../examples/networking.md#custom-subnetwork).

## Combining both overrides

`enableExternalIPAccess` and `subnetwork` can be set together on the same interface entry:

```yaml
networkConfig:
  networkInterfaces:
    - enableExternalIPAccess: false
      subnetwork: regions/us-central1/subnetworks/private-nodes
```

## Multi-interface nodes

`networkInterfaces` is an ordered list matched to the node pool template interfaces by position (index 0 = primary interface). Interfaces in the template that have no corresponding entry in the list are left unchanged. The list is capped at 8 entries matching GCP's limit on network interfaces per instance.

```yaml
networkConfig:
  networkInterfaces:
    - enableExternalIPAccess: false   # primary interface: no external IP
    - subnetwork: regions/us-central1/subnetworks/secondary-net  # secondary interface: different subnet
```

## Relationship to `networkTags` and `subnetRangeName`

| Field                                                      | Scope                     | Purpose                          |
|------------------------------------------------------------|---------------------------|----------------------------------|
| `networkTags`                                              | Instance (all interfaces) | GCP firewall rule targets        |
| `networkConfig.networkInterfaces[].subnetwork`             | Per interface             | Which subnetwork to attach       |
| `networkConfig.networkInterfaces[].enableExternalIPAccess` | Per interface             | Whether to assign an external IP |
| `subnetRangeName`                                          | Per interface (all)       | Secondary IP range for pod IPs   |

`networkTags` is intentionally top-level because GCP's Compute API places tags on the `Instance` resource, not on individual `NetworkInterface` objects — they apply to all interfaces on the instance.

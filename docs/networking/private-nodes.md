# Private Nodes

This page explains how to control external IP assignment and subnetwork placement for nodes provisioned by Karpenter.

## How Karpenter manages node pool templates

When Karpenter starts, it creates four zero-node GKE node pools that act as instance templates:

| Pool name                | Image                  | Architecture |
|--------------------------|------------------------|--------------|
| `karpenter-default`      | Container-Optimized OS | amd64        |
| `karpenter-ubuntu`       | Ubuntu                 | amd64        |
| `karpenter-cos-arm64`    | Container-Optimized OS | arm64        |
| `karpenter-ubuntu-arm64` | Ubuntu                 | arm64        |

These pools have `InitialNodeCount: 0` — they hold no running nodes and exist purely to give Karpenter a GKE-managed instance template to clone from. The network configuration of these templates comes from the GKE cluster itself, not from Karpenter.

The arm64 pools (`karpenter-cos-arm64` and `karpenter-ubuntu-arm64`) are created on a best-effort basis. If the cluster's region does not support arm64 machine types, those pools are skipped and arm64 provisioning is disabled for that image family.

## Cluster-level private nodes

Karpenter reads `PrivateClusterConfig.EnablePrivateNodes` from the cluster API and automatically omits the `ONE_TO_ONE_NAT` access config from all **provisioned instances** on private clusters — no extra NodeClass configuration is required for this.

However, Karpenter still creates bootstrap node pool templates at startup. On clusters that enforce private nodes via org policy (e.g. `container.managed.enablePrivateNodes`), this creation request may be rejected by GKE, preventing Karpenter from starting. Full support for such clusters is tracked in [#230](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/230), which eliminates bootstrap pool creation entirely.

To override the automatic external-IP behaviour on a per-NodeClass basis:

```yaml
networkConfig:
  enablePrivateNodes: true   # force private, even on a public cluster
  # or
  enablePrivateNodes: false  # force public, even on a private cluster
```

## Selectively disabling external IPs via NodeClass

On a standard GKE cluster (public nodes), you may want Karpenter to provision nodes without external IPs while leaving other node pools public. Set `networkConfig.enablePrivateNodes: true` on a `GCENodeClass`:

```yaml
networkConfig:
  enablePrivateNodes: true
```

Setting `enablePrivateNodes: true` removes the `ONE_TO_ONE_NAT` access config from every node Karpenter provisions via that `GCENodeClass`. This mirrors the `enable_private_nodes` field in the Terraform `google_container_node_pool` resource's `network_config` block.

### Prerequisites

1. **Cloud NAT** — nodes without an external IP have no outbound internet access unless Cloud NAT is configured on the VPC. Without it, nodes cannot pull container images from public registries or reach GCP APIs. See: [Using Cloud NAT with GKE](https://cloud.google.com/nat/docs/gke-example)

2. **VPC-native cluster** — the cluster must use alias IP ranges (VPC-native mode). This is the default for all new GKE clusters. See: [VPC-native clusters](https://cloud.google.com/kubernetes-engine/docs/concepts/alias-ips)

3. **Node access** — without an external IP, direct SSH and `kubectl exec` require [Identity-Aware Proxy (IAP)](https://cloud.google.com/iap/docs/using-tcp-forwarding) or a bastion host.

### Example

See [NodePool examples — Private nodes](../examples/networking.md#private-nodes-no-external-ip).

## Overriding the subnetwork

By default, nodes are placed in the cluster's primary subnetwork (read from `cluster.NetworkConfig.Subnetwork`). Use `networkConfig.subnetwork` to place Karpenter nodes in a different subnetwork — for example one with a narrower CIDR or separate firewall rules. This mirrors the `subnetwork` field in the Terraform `google_container_node_pool` `network_config` block.

```yaml
networkConfig:
  subnetwork: regions/us-central1/subnetworks/karpenter-nodes
```

The value must be a [self-link or partial URL](https://cloud.google.com/compute/docs/reference/rest/v1/instances#networkinterface):

- Partial: `regions/REGION/subnetworks/NAME`
- Full: `https://www.googleapis.com/compute/v1/projects/PROJECT/regions/REGION/subnetworks/NAME`

### Example

See [NodePool examples — Custom subnetwork](../examples/networking.md#custom-subnetwork).

## Combining both overrides

`enablePrivateNodes` and `subnetwork` can be set together:

```yaml
networkConfig:
  enablePrivateNodes: true
  subnetwork: regions/us-central1/subnetworks/private-nodes
```

## Multi-interface nodes

Use `additionalNetworkInterfaces` to attach secondary network interfaces. Each entry must specify a `subnetwork`. The `network` field is optional and defaults to the cluster's VPC. This mirrors the `additional_node_network_configs` block in the Terraform `google_container_node_pool` `network_config`.

```yaml
networkConfig:
  enablePrivateNodes: true
  additionalNetworkInterfaces:
    - subnetwork: regions/us-central1/subnetworks/secondary-net  # network defaults to cluster VPC
    - network: projects/p/global/networks/other-vpc               # explicit network override
      subnetwork: regions/us-central1/subnetworks/other-net
```

Secondary interfaces inherit the `enablePrivateNodes` setting from the top level.

## Relationship to `networkTags` and `subnetRangeName`

| Field                                                    | Scope                     | Purpose                          |
|----------------------------------------------------------|---------------------------|----------------------------------|
| `networkTags`                                            | Instance (all interfaces) | GCP firewall rule targets        |
| `networkConfig.subnetwork`                               | Primary interface         | Which subnetwork to attach       |
| `networkConfig.enablePrivateNodes`                       | All interfaces            | Whether to assign an external IP |
| `networkConfig.additionalNetworkInterfaces[].network`    | Per secondary interface   | Which VPC network to attach      |
| `networkConfig.additionalNetworkInterfaces[].subnetwork` | Per secondary interface   | Which subnetwork to attach       |
| `subnetRangeName`                                        | Primary interface         | Secondary IP range for pod IPs   |

`networkTags` is intentionally top-level because GCP's Compute API places tags on the `Instance` resource, not on individual `NetworkInterface` objects — they apply to all interfaces on the instance.

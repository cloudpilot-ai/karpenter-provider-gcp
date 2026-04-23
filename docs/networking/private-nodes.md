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

However, Karpenter still creates bootstrap node pool templates at startup. On clusters that enforce private nodes via org policy (e.g. `container.managed.enablePrivateNodes`), this creation request may be rejected by GKE, preventing Karpenter from starting. Full support for such clusters is tracked in [#263](https://github.com/cloudpilot-ai/karpenter-provider-gcp/pull/263), which eliminates bootstrap pool creation entirely.

To override the automatic external-IP behaviour on a per-NodeClass basis:

```yaml
networkConfig:
  networkInterface:
    enableExternalIPAccess: false  # force private, even on a public cluster
    # or
    enableExternalIPAccess: true   # force public, even on a private cluster
```

## Selectively disabling external IPs via NodeClass

On a standard GKE cluster (public nodes), you may want Karpenter to provision nodes without external IPs while leaving other node pools public. Use `networkConfig.networkInterface` on a `GCENodeClass` to override the default derived from the cluster:

```yaml
networkConfig:
  networkInterface:
    enableExternalIPAccess: false
```

Setting `enableExternalIPAccess: false` removes the `ONE_TO_ONE_NAT` access config from the primary interface of every node Karpenter provisions via that `GCENodeClass`.

### Prerequisites

1. **Cloud NAT** — nodes without an external IP have no outbound internet access unless Cloud NAT is configured on the VPC. Without it, nodes cannot pull container images from public registries or reach GCP APIs. See: [Using Cloud NAT with GKE](https://cloud.google.com/nat/docs/gke-example)

2. **VPC-native cluster** — the cluster must use alias IP ranges (VPC-native mode). This is the default for all new GKE clusters. See: [VPC-native clusters](https://cloud.google.com/kubernetes-engine/docs/concepts/alias-ips)

3. **Node access** — without an external IP, direct SSH and `kubectl exec` require [Identity-Aware Proxy (IAP)](https://cloud.google.com/iap/docs/using-tcp-forwarding) or a bastion host.

### Example

See [NodePool examples — Private nodes](../examples/networking.md#private-nodes-no-external-ip).

## Overriding the subnetwork

By default, nodes are placed in the cluster's primary subnetwork (read from `cluster.NetworkConfig.Subnetwork`). Use `networkConfig.networkInterface.subnetwork` to place Karpenter nodes in a different subnetwork — for example one with a narrower CIDR or separate firewall rules.

```yaml
networkConfig:
  networkInterface:
    subnetwork: regions/us-central1/subnetworks/karpenter-nodes
```

The value must be a [self-link or partial URL](https://cloud.google.com/compute/docs/reference/rest/v1/instances#networkinterface):

- Partial: `regions/REGION/subnetworks/NAME`
- Full: `https://www.googleapis.com/compute/v1/projects/PROJECT/regions/REGION/subnetworks/NAME`

### Example

See [NodePool examples — Custom subnetwork](../examples/networking.md#custom-subnetwork).

## Combining both overrides

`enableExternalIPAccess` and `subnetwork` can be set together on the primary interface:

```yaml
networkConfig:
  networkInterface:
    enableExternalIPAccess: false
    subnetwork: regions/us-central1/subnetworks/private-nodes
```

## Multi-interface nodes

Use `additionalNetworkInterfaces` to attach secondary network interfaces. Each entry must specify a `subnetwork` — this mirrors GKE's `additionalNodeNetworkConfigs` requirement and is enforced by the CRD schema.

```yaml
networkConfig:
  networkInterface:
    enableExternalIPAccess: false          # primary: no external IP
  additionalNetworkInterfaces:
    - subnetwork: regions/us-central1/subnetworks/secondary-net  # secondary interface
```

The primary interface always uses the cluster's network and subnetwork (unless overridden via `networkInterface`). Secondary interfaces inherit the cluster's VPC network but require an explicit subnetwork.

## Relationship to `networkTags` and `subnetRangeName`

| Field                                                                | Scope                     | Purpose                          |
|----------------------------------------------------------------------|---------------------------|----------------------------------|
| `networkTags`                                                        | Instance (all interfaces) | GCP firewall rule targets        |
| `networkConfig.networkInterface.subnetwork`                          | Primary interface         | Which subnetwork to attach       |
| `networkConfig.networkInterface.enableExternalIPAccess`              | Primary interface         | Whether to assign an external IP |
| `networkConfig.additionalNetworkInterfaces[].subnetwork`             | Per secondary interface   | Which subnetwork to attach       |
| `networkConfig.additionalNetworkInterfaces[].enableExternalIPAccess` | Per secondary interface   | Whether to assign an external IP |
| `subnetRangeName`                                                    | Primary interface         | Secondary IP range for pod IPs   |

`networkTags` is intentionally top-level because GCP's Compute API places tags on the `Instance` resource, not on individual `NetworkInterface` objects — they apply to all interfaces on the instance.

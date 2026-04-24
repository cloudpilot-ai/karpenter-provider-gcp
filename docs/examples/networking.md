# Networking examples

## Private nodes (no external IP)

Provision nodes without a public IP address.

See [`examples/nodeclass/private-nodes-gcenodeclass.yaml`](https://github.com/cloudpilot-ai/karpenter-provider-gcp/blob/main/examples/nodeclass/private-nodes-gcenodeclass.yaml).

```yaml
networkConfig:
  networkInterfaces:
    - enableExternalIPAccess: false
```

Setting `enableExternalIPAccess: false` removes the `ONE_TO_ONE_NAT` access config from the primary interface of every node Karpenter provisions via that `GCENodeClass`.

**Prerequisites:**

- **Cloud NAT** — nodes without an external IP need Cloud NAT for outbound internet access (container image pulls, GCP API calls). See: [Using Cloud NAT with GKE](https://cloud.google.com/nat/docs/gke-example)
- **VPC-native cluster** — alias IP ranges (VPC-native mode) required. Default for all new GKE clusters. See: [VPC-native clusters](https://cloud.google.com/kubernetes-engine/docs/concepts/alias-ips)
- **Node access** — SSH and `kubectl exec` require [IAP TCP forwarding](https://cloud.google.com/iap/docs/using-tcp-forwarding) or a bastion host.

**Cluster-level private nodes** (`DefaultEnablePrivateNodes: true` or `PrivateClusterConfig.EnablePrivateNodes: true`) are detected automatically — no extra configuration needed.

## Custom subnetwork

Place Karpenter nodes in a specific subnetwork, rather than the cluster's default subnet.

See [`examples/nodeclass/subnetwork-override-gcenodeclass.yaml`](https://github.com/cloudpilot-ai/karpenter-provider-gcp/blob/main/examples/nodeclass/subnetwork-override-gcenodeclass.yaml).

```yaml
networkConfig:
  networkInterfaces:
    - subnetwork: regions/us-central1/subnetworks/karpenter-nodes
```

The value must be a [self-link or partial URL](https://cloud.google.com/compute/docs/reference/rest/v1/instances#networkinterface):

- Partial: `regions/REGION/subnetworks/NAME`
- Full: `https://www.googleapis.com/compute/v1/projects/PROJECT/regions/REGION/subnetworks/NAME`

Combine with `enableExternalIPAccess: false` to put nodes in a private subnet:

```yaml
networkConfig:
  networkInterfaces:
    - enableExternalIPAccess: false
      subnetwork: regions/us-central1/subnetworks/private-nodes
```

## Custom pod IP range

When a cluster has multiple secondary IP ranges, direct pod IPs to a specific range:

```yaml
spec:
  subnetRangeName: karpenter-pods
```

## Multi-interface nodes

`networkInterfaces` is an ordered list matched to the bootstrap pool template interfaces by position (index 0 = primary interface). Unspecified interfaces are left unchanged. The list is capped at 8 entries (GCP limit).

```yaml
networkConfig:
  networkInterfaces:
    - enableExternalIPAccess: false          # primary: no external IP
    - subnetwork: regions/us-central1/subnetworks/secondary-net  # secondary: different subnet
```

## Field reference

| Field                                                      | Scope                     | Purpose                          |
|------------------------------------------------------------|---------------------------|----------------------------------|
| `networkTags`                                              | Instance (all interfaces) | GCP firewall rule targets        |
| `networkConfig.networkInterfaces[].subnetwork`             | Per interface             | Which subnetwork to attach       |
| `networkConfig.networkInterfaces[].enableExternalIPAccess` | Per interface             | Whether to assign an external IP |
| `subnetRangeName`                                          | Per interface (all)       | Secondary IP range for pod IPs   |

`networkTags` is top-level because GCP places tags on the `Instance` resource, not on individual `NetworkInterface` objects.

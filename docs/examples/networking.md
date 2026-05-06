# Networking examples

## Private nodes (no external IP)

Provision nodes without a public IP address. Requires [Cloud NAT](https://cloud.google.com/nat/docs/gke-example) for outbound internet access.

See [`examples/nodeclass/private-nodes-gcenodeclass.yaml`](https://github.com/cloudpilot-ai/karpenter-provider-gcp/blob/main/examples/nodeclass/private-nodes-gcenodeclass.yaml).

See [Private Nodes](../networking/private-nodes.md) for prerequisites and caveats.

## Custom subnetwork

Place Karpenter nodes in a specific subnetwork, rather than the cluster's default subnet.

See [`examples/nodeclass/subnetwork-override-gcenodeclass.yaml`](https://github.com/cloudpilot-ai/karpenter-provider-gcp/blob/main/examples/nodeclass/subnetwork-override-gcenodeclass.yaml).

Combine with `enablePrivateNodes: true` to put nodes in a private subnet:

```yaml
networkConfig:
  enablePrivateNodes: true
  subnetwork: regions/us-central1/subnetworks/private-nodes
```

## Custom pod IP range

GKE VPC-native clusters use [alias IP ranges](https://cloud.google.com/kubernetes-engine/docs/concepts/alias-ips) to assign pod IP addresses. Each node receives a slice of the cluster's secondary IP range, and pods on that node get IPs from that slice.

By default, Karpenter uses the cluster's primary pod secondary range (`ClusterSecondaryRangeName` from the cluster's IP allocation policy). Use `subnetRangeName` to direct pods to a different secondary range on the subnet.

### When to use this

- **Multi-tenant isolation**: Route different workload tiers to separate IP ranges for network policy or billing separation.
- **IP exhaustion**: Direct high-density NodePools to a larger secondary range to avoid running out of pod IPs.
- **Network segmentation**: Use distinct ranges for compliance or firewall rule scoping.

### Finding your secondary range names

**GCP Console**: VPC Network → Subnets → click your subnet → view "Secondary IP ranges".

**gcloud CLI**:

```bash
gcloud compute networks subnets describe SUBNET_NAME \
  --region=REGION \
  --format="table(secondaryIpRanges.rangeName, secondaryIpRanges.ipCidrRange)"
```

### Configuration

```yaml
apiVersion: karpenter.k8s.gcp/v1alpha1
kind: GCENodeClass
metadata:
  name: custom-pod-range
spec:
  imageSelectorTerms:
    - alias: ContainerOptimizedOS@latest
  subnetRangeName: karpenter-pods
  disks:
    - category: pd-balanced
      sizeGiB: 60
      boot: true
```

See [`examples/nodeclass/subnet-range-gcenodeclass.yaml`](https://github.com/cloudpilot-ai/karpenter-provider-gcp/blob/main/examples/nodeclass/subnet-range-gcenodeclass.yaml).

### Relationship to `networkConfig.subnetwork`

These fields control different aspects of node networking:

| Field | Controls | GCP API mapping |
|-------|----------|-----------------|
| `subnetRangeName` | Which secondary IP range is used for **pod IPs** (alias IPs) | `AliasIpRange.SubnetworkRangeName` |
| `networkConfig.subnetwork` | Which subnetwork the **node** is placed in | `NetworkInterface.Subnetwork` |

Combine both to place nodes in a specific subnet while directing pods to a non-default secondary range within that subnet.

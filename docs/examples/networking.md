# Networking examples

## Private nodes (no external IP)

Provision nodes without a public IP address. Requires [Cloud NAT](https://cloud.google.com/nat/docs/gke-example) for outbound internet access.

See [`examples/nodeclass/private-nodes-gcenodeclass.yaml`](https://github.com/cloudpilot-ai/karpenter-provider-gcp/blob/main/examples/nodeclass/private-nodes-gcenodeclass.yaml).

See [Private Nodes](../networking/private-nodes.md) for prerequisites and caveats.

## Custom subnetwork

Place Karpenter nodes in a specific subnetwork, rather than the cluster's default subnet.

See [`examples/nodeclass/subnetwork-override-gcenodeclass.yaml`](https://github.com/cloudpilot-ai/karpenter-provider-gcp/blob/main/examples/nodeclass/subnetwork-override-gcenodeclass.yaml).

Combine with `enableExternalIPAccess: false` to put nodes in a private subnet:

```yaml
networkConfig:
  networkInterface:
    enableExternalIPAccess: false
    subnetwork: regions/us-central1/subnetworks/private-nodes
```

## Custom pod IP range

When a cluster has multiple secondary IP ranges, direct pod IPs to a specific range:

```yaml
spec:
  subnetRangeName: karpenter-pods
```

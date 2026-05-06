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

Direct pods to a specific [secondary IP range](https://cloud.google.com/kubernetes-engine/docs/concepts/alias-ips) instead of the cluster default.

```yaml
spec:
  subnetRangeName: karpenter-pods
```

See [`examples/nodeclass/subnet-range-gcenodeclass.yaml`](https://github.com/cloudpilot-ai/karpenter-provider-gcp/blob/main/examples/nodeclass/subnet-range-gcenodeclass.yaml).

> **Note**: `subnetRangeName` controls pod IPs (alias IPs). To change the node's subnet, use `networkConfig.subnetwork`.

On clusters with [additional pod ranges](https://cloud.google.com/kubernetes-engine/docs/how-to/multi-pod-cidr) (`additionalPodRangesConfig`), Karpenter always allocates from the cluster's default pod range and does not spill over to additional ranges automatically. If your default range is near exhaustion, set `subnetRangeName` to pin allocation to a specific secondary range.

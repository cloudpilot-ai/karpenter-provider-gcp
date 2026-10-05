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

Direct pods to a specific [secondary IP range](https://cloud.google.com/kubernetes-engine/docs/concepts/alias-ips) instead of using the discovered cluster ranges.

```yaml
spec:
  subnetRangeNames: [karpenter-pods]
```

To restrict allocation to several ranges (for example, the cluster default plus selected [additional pod ranges](https://cloud.google.com/kubernetes-engine/docs/how-to/multi-pod-cidr)), list them on one NodeClass. At launch Karpenter prefers the range with the most Compute-reported free IPv4 addresses, and retries remaining names if Compute returns IP space exhausted.

```yaml
spec:
  subnetRangeNames:
    - gke-example-pods
    - gke-example-additional
```

See [`examples/nodeclass/subnet-ranges-gcenodeclass.yaml`](https://github.com/cloudpilot-ai/karpenter-provider-gcp/blob/main/examples/nodeclass/subnet-ranges-gcenodeclass.yaml).

The deprecated `subnetRangeName` and `subnetRangeNames` are mutually exclusive. Either field completely replaces the discovered range list; it is not merged with cluster ranges. Resolved names and optional integer `totalFreeIP` counts appear on `status.subnetRanges`. Zero means a reported zero; an omitted count means unknown capacity.

> **Note**: These fields control pod IPs (alias IPs). To change the node's subnet, use `networkConfig.subnetwork` and specify pod range names belonging to that subnetwork.

If neither field is set, Karpenter discovers the cluster's default pod range plus additional pod ranges on its primary subnetwork. Names from GKE's configured range list and utilization metadata are combined without duplicates. Ranges on separate additional subnetworks are not included. Selection uses descending `subnetworks.get` free-IP counts and IP-exhaustion fallback, not GKE's node-pool allocation algorithm. Counts are cached for one minute; unknown counts follow known counts, with candidate order breaking ties. If the capacity read fails, launch preserves candidate order without counts. Free IPs do not guarantee a contiguous pod CIDR block.

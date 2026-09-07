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

To spill over across several ranges (the cluster default plus [additional pod ranges](https://cloud.google.com/kubernetes-engine/docs/how-to/multi-pod-cidr)), list them on one NodeClass. At launch Karpenter picks the range with the lowest GKE-reported utilization, and retries remaining names if Compute returns IP space exhausted.

```yaml
spec:
  subnetRangeNames:
    - gke-example-pods
    - gke-example-additional
```

See [`examples/nodeclass/subnet-ranges-gcenodeclass.yaml`](https://github.com/cloudpilot-ai/karpenter-provider-gcp/blob/main/examples/nodeclass/subnet-ranges-gcenodeclass.yaml).

`subnetRangeName` and `subnetRangeNames` are mutually exclusive. Resolved names and utilization appear on `status.subnetRanges`.

> **Note**: These fields control pod IPs (alias IPs). To change the node's subnet, use `networkConfig.subnetwork`.

If neither field is set, Karpenter allocates from the cluster's default pod range only and does not include additional ranges automatically.

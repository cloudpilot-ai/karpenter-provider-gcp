# Karpenter GCP

Karpenter GCP is an open-source node provisioning provider for Kubernetes that runs on [Google Kubernetes Engine (GKE)](https://cloud.google.com/kubernetes-engine). It extends [Karpenter](https://karpenter.sh) with GCP-specific functionality, enabling cost-efficient and low-latency autoscaling for GKE clusters.

## How it works

Karpenter watches for pods that the Kubernetes scheduler cannot place, evaluates their resource and scheduling requirements, and provisions GCE instances that satisfy those constraints — typically within seconds. When the nodes are no longer needed, Karpenter removes them.

```
Unschedulable pod → Karpenter evaluates requirements → GCE instance launched → Pod scheduled
```

Key capabilities:

- **Bin-packing** — selects instance types that tightly fit pod resource requests, reducing waste
- **Spot and on-demand** — provisions Spot VMs when available, falls back to on-demand automatically
- **Multi-arch** — supports amd64 and arm64 (Google Axion / Ampere Altra) nodes
- **Multiple image families** — Container-Optimized OS and Ubuntu
- **Node consolidation** — replaces underutilised nodes with fewer, better-fitting ones
- **Drift detection** — replaces nodes that have drifted from their desired configuration

## GCP-specific features

- **GCENodeClass** — a custom resource that captures all GCP-specific node configuration: image family, disk type and size, service account, network tags, Shielded VM settings, kubelet configuration, and network overrides
- **Pool-discovery bootstrap** — Karpenter discovers an existing RUNNING GKE node pool to obtain bootstrap metadata (kubelet config, network settings, instance templates), ensuring provisioned nodes are fully GKE-compatible. No dedicated Karpenter-owned node pool is required; Karpenter reuses any pool already present in the cluster
- **Node repair policies** — integrates with GKE's node problem detection to trigger replacement of unhealthy nodes

## Known limitations

- **GKE Standard mode only** — GKE Autopilot manages its own node pools and does not allow external node provisioners.
- **Linux nodes only** — Container-Optimized OS and Ubuntu are supported. Windows nodes are not.
- **Kubernetes 1.28+** — earlier versions are not tested or supported.
- **VPC-native (alias IP) networking required** — legacy routes-based networking is untested.

## Getting started

- [Installation](getting-started/installation.md) — deploy Karpenter on a GKE cluster using Helm
- [Quick start](getting-started/quick-start.md) — create your first NodePool and GCENodeClass, and trigger node provisioning
- [Terraform](https://github.com/cloudpilot-ai/karpenter-provider-gcp/tree/main/deploy/terraform) — provision the full GCP infrastructure (VPC, GKE cluster, service accounts) with Terraform

## Reference

- [GCENodeClass](reference/gcenodeclass.md) — full field reference for the `GCENodeClass` resource
- [NodePool](reference/nodepool.md) — full field reference for the `NodePool` resource
- [Bootstrap pool selection](bootstrap-pool.md) — how Karpenter selects the GKE node pool used as its bootstrap metadata source

## Examples

- [Default](examples/default.md) — spot and on-demand, multi-zone
- [arm64](examples/arm64.md) — Google Axion and Ampere Altra nodes
- [Ubuntu](examples/ubuntu.md) — Ubuntu image family
- [GPU](examples/gpu.md) — GPU workloads
- [Networking](examples/networking.md) — private nodes, custom subnetwork, pod IP range
- [Advanced](examples/advanced.md) — kubelet config, Shielded VM, metadata, secondary disk, multiple pools

## Community

- [Slack](https://cloudpilotaicommunity.slack.com/archives/C093V65481H)
- [Discord](https://discord.gg/WxFWc87QWr)
- [GitHub Issues](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues)

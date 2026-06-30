# Karpenter GCP review guidance

When reviewing provider changes, check that package boundaries stay clear:

- `pkg/providers/instance` owns GCE VM lifecycle, instance labels/tags, scheduling, capacity errors, and zone operations.
- `pkg/providers/nodepooltemplate` owns GKE bootstrap source pool selection, fallback pool creation, and source instance-template discovery.
- `pkg/providers/gke` owns shared GKE cluster facts such as cluster config, server config, cluster identity, and zone resolution.
- `pkg/metadata` owns pure typed GKE bootstrap metadata modeling, parsing, typed mutation helpers, and Compute API rendering only.

For `pkg/metadata`, flag any change that adds GCP/Kubernetes API calls, source node-pool or instance-template discovery, GCE VM lifecycle decisions, scheduler/capacity policy, or provider-owned bootstrap decisions. Provider policy such as labels, taints, provisioning model, GPU behavior, disk compatibility, secondary boot disks, kubelet defaults, Spot shutdown behavior, and NodeClass overlays belongs in provider packages and should be passed into `pkg/metadata` through typed helpers.

Flag changes that duplicate GKE API reads across providers, let bootstrap metadata helpers perform API calls, mutate caller-owned Compute metadata templates in place, or mix Kubernetes object metadata with GCE instance labels/tags.

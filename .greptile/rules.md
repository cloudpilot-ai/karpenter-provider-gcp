# Karpenter GCP review guidance

When reviewing provider changes, check that package boundaries stay clear:

- `pkg/providers/instance` owns GCE VM lifecycle, instance labels/tags, scheduling, capacity errors, and zone operations.
- `pkg/providers/nodepooltemplate` owns GKE bootstrap source pool selection, fallback pool creation, and source instance-template discovery.
- `pkg/providers/gke` owns shared GKE cluster facts such as cluster config, server config, cluster identity, and zone resolution.
- `pkg/providers/metadata` owns pure GKE bootstrap metadata transformations only.

Flag changes that duplicate GKE API reads across providers, let bootstrap metadata helpers perform API calls, or mix Kubernetes object metadata with GCE instance labels/tags.

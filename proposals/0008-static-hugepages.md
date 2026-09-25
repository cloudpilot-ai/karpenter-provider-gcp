# Proposal: Static Hugepages for GCE NodeClasses

- **Status**: Draft
- **Authors**: @mdlayher
- **Created**: 2026-09-25
- **Related Issues**: N/A

---

## Summary

GKE node pools can allocate static 2 MiB hugepages at boot through `linuxNodeConfig.hugepages.hugepageSize2m`. Karpenter-GCP has no equivalent, so a node that Karpenter launches cannot serve pods that request `hugepages-2Mi`.

This proposal adds `GCENodeClass.spec.linuxNodeConfig.hugepages.hugepageSize2m`. When it is set, the instance provider writes the same `kube-env` entries that GKE writes for a hugepages node pool, and the GKE node bootstrap allocates the pages before the kubelet starts. The field name and shape mirror the GKE v1 node pool API field [`LinuxNodeConfig.hugepages`](https://docs.cloud.google.com/kubernetes-engine/docs/reference/rest/v1/LinuxNodeConfig#HugepagesConfig).

The scheduling simulation must also know that these nodes have `hugepages-2Mi` capacity. This proposal describes two options: the instance type provider adds the capacity from the `GCENodeClass` (Option A), or operators advertise it with a `NodeOverlay`, as AWS users do today (Option B). See [Scheduling Capacity](#scheduling-capacity).

---

## Motivation

### Problem Statement

Databases such as PostgreSQL use hugepages to reduce TLB pressure for large shared memory segments. A pod requests them as an extended resource:

```yaml
resources:
  requests:
    hugepages-2Mi: 8Gi
    memory: 16Gi
  limits:
    hugepages-2Mi: 8Gi
    memory: 16Gi
```

The kubelet only reports `hugepages-2Mi` capacity when the kernel has allocated the pages. On GKE this happens at boot when the node pool sets `linuxNodeConfig.hugepages`. A node that Karpenter-GCP launches copies its bootstrap metadata from a source node pool and has no way to request hugepages, so these pods cannot run on Karpenter nodes.

The usual workarounds do not apply:

- `spec.metadata` rejects `startup-script`, `user-data`, and `kube-env`, because the provider owns them.
- `GCENodeClass` has no `userData`, and Karpenter-GCP copies the node bootstrap metadata from the source node pool. On AWS, users allocate hugepages outside Karpenter, with a `userData` script (for example `sysctl -w vm.nr_hugepages=<pages>`) or a custom AMI. Neither path exists for a `GCENodeClass`.
- A DaemonSet that allocates pages after the node joins races the kubelet, which has already reported the node capacity. The DaemonSet must restart the kubelet and taint the node until it does.

### Goals

- Allocate a static number of 2 MiB hugepages on every node that a `GCENodeClass` launches.
- Use the same boot mechanism as GKE node pools, so the result matches a GKE hugepages node.
- Keep the API shape and field name of the GKE node pool API.
- Leave existing `GCENodeClass` objects and their drift hashes unchanged.

### Non-Goals

- 1 GiB hugepages. The same shape can add `hugepageSize1g` later. See [Future Direction](#future-direction).
- Dynamic hugepages, where the page count depends on the pods that trigger provisioning.
- Other `linuxNodeConfig` fields such as `sysctls` or `cgroupMode`.

---

## Proposal

### Overview

Before: a pod that requests `hugepages-2Mi` stays pending on Karpenter-GCP. No `GCENodeClass` can launch a node with hugepages.

After: an operator creates a `GCENodeClass` with the page count:

```yaml
apiVersion: karpenter.k8s.gcp/v1alpha1
kind: GCENodeClass
metadata:
  name: hugepages
spec:
  imageSelectorTerms:
    - alias: Ubuntu@latest
  linuxNodeConfig:
    hugepages:
      hugepageSize2m: 4096
```

A NodePool that references the class launches nodes with 4096 pages. The kubelet on those nodes reports `hugepages-2Mi: 8Gi` capacity. With Option A the scheduling simulation sees the same capacity with no other object. With Option B the operator also creates a `NodeOverlay`.

### Design Details

#### API

```go
type GCENodeClassSpec struct {
	// ...
	// LinuxNodeConfig configures the Linux kernel of provisioned nodes.
	// Mirrors GKE node pool NodeConfig.linuxNodeConfig.
	// +optional
	LinuxNodeConfig *LinuxNodeConfig `json:"linuxNodeConfig,omitempty"`
}

type LinuxNodeConfig struct {
	// Hugepages configures the static hugepages that the node allocates at boot.
	// Mirrors GKE node pool LinuxNodeConfig.hugepages.
	// +optional
	Hugepages *HugepagesConfig `json:"hugepages,omitempty"`
}

type HugepagesConfig struct {
	// HugepageSize2m is the number of 2 MiB hugepages to allocate.
	// +kubebuilder:validation:Minimum=1
	// +optional
	HugepageSize2m *int32 `json:"hugepageSize2m,omitempty"`
}
```

`linuxNodeConfig` leaves room for other GKE Linux node options. The names follow the GKE v1 REST API: [`LinuxNodeConfig.hugepages`](https://docs.cloud.google.com/kubernetes-engine/docs/reference/rest/v1/LinuxNodeConfig#HugepagesConfig) holds a `HugepagesConfig` with `hugepageSize2m` and `hugepageSize1g`. That API is stable, so the field name does not need to change later.

#### Bootstrap

A GKE node pool with `linuxNodeConfig.hugepages.hugepageSize2m: 256000` produces a node whose `kube-env` differs from a node without it in exactly two entries:

```yaml
HUGEPAGE_2M: "256000"
ENABLE_CONTAINERD_HUGETLB_CONTROLLER: "true"
```

The GKE node bootstrap reads `HUGEPAGE_2M` and allocates the pages before it starts the kubelet. The provider writes both entries, so a Karpenter node matches a GKE hugepages node.

A new `patchHugepagesKubeEnv` runs in `setupInstanceMetadata`, next to `patchSecondaryBootDisksKubeEnv`:

- When `hugepageSize2m` is set, it sets both entries.
- When it is not set, it removes `HUGEPAGE_2M`. The source node pool can allocate hugepages, and a class without the field must not inherit its page count.

#### Drift

`GCENodeClass.Hash()` uses `IgnoreZeroValue` and `ZeroNil`, so a nil `linuxNodeConfig` does not change the hash of an existing class. `GCENodeClassHashVersion` does not change. A change to `hugepageSize2m` changes the hash, so the nodes of that class drift and are replaced. The page count is only applied at boot, so this is the correct behavior.

#### Scheduling Capacity

On the node, no extra work is needed. The kubelet reports `hugepages-2Mi` capacity and subtracts the pages from allocatable memory.

The scheduling simulation is separate. It must see `hugepages-2Mi` capacity on the instance types of the class, or it does not launch a node for a pod that requests hugepages. Karpenter core already handles the memory side: `InstanceType.computeAllocatable` subtracts every `hugepages-*` capacity entry from allocatable memory, and clamps the result at zero. This works the same whether the entry comes from the provider or from a `NodeOverlay`. The two options differ only in where the capacity entry comes from.

##### Option A: the instance type provider adds the capacity

`computeCapacity` in `pkg/providers/instancetype/types.go` adds `hugepages-2Mi: hugepageSize2m * 2Mi` when the field is set:

```go
if c := nodeClass.Spec.LinuxNodeConfig; c != nil && c.Hugepages != nil && c.Hugepages.HugepageSize2m != nil {
	pages := int64(*c.Hugepages.HugepageSize2m)
	resourceList[corev1.ResourceHugePagesPrefix+"2Mi"] = *resource.NewQuantity(pages*2*1024*1024, resource.BinarySI)
}
```

Instance types are already computed for each `GCENodeClass`. `getStaticInstanceTypes` keys its cache on a hash of `KubeletConfiguration` and `Disks`, and `maxPods`, reserved resources, and local SSD capacity come from the class the same way. The cache key adds a hash of `LinuxNodeConfig`, so two classes that differ only in page count do not share instance types.

- The page count is written in one place, so the scheduling simulation always matches the node.
- A machine type that is too small for the pages has zero allocatable memory in the simulation, so Karpenter does not choose it for a pod that requests memory. NodePool requirements do not need to exclude small machine types.
- A `NodeOverlay` with the same capacity still works. The overlay replaces the entry with the same value, and core subtracts the hugepages from memory once for each resource name.
- More code in the provider: the capacity entry, the cache key, and their tests.

##### Option B: operators advertise the capacity with a `NodeOverlay`

The provider only writes the `kube-env` entries. The operator creates a `NodeOverlay` for the NodePools that use the class:

```yaml
apiVersion: karpenter.sh/v1alpha1
kind: NodeOverlay
metadata:
  name: hugepages
spec:
  requirements:
    - key: karpenter.sh/nodepool
      operator: In
      values: [hugepages]
  capacity:
    hugepages-2Mi: 8Gi
```

- No instance type changes in the provider.
- The same pattern as the AWS provider. `EC2NodeClass` has no hugepages field: users allocate pages with a `userData` script or a custom AMI, which the provider cannot read. A `NodeOverlay` is therefore the only way to advertise the capacity on AWS. The AWS e2e suite uses this pattern, and the Nitro Enclaves guide documents it for `hugepages-1Gi`.
- The page count is written twice: `hugepageSize2m` on the class and `hugepages-2Mi` on the overlay. If the two do not match, the simulation launches nodes that cannot fit the pod, or does not launch nodes that can.
- `NodeOverlay` is `v1alpha1` in Karpenter core and needs the `NodeOverlay` feature gate. A fixed overlay also has the limits described in [kubernetes-sigs/karpenter#3297](https://github.com/kubernetes-sigs/karpenter/issues/3297).

---

## Risks and Mitigations

| Risk                                                         | Likelihood | Impact                                                                   | Mitigation                                                                                                                                                                                                                                                                                                                                                                                                |
|--------------------------------------------------------------|------------|--------------------------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| GKE changes the `HUGEPAGE_2M` `kube-env` entry               | Low        | Nodes boot without hugepages; pods that request them stay pending        | The node pool API field is stable GKE v1 API, but the `kube-env` entry is an undocumented bootstrap detail. Karpenter-GCP already depends on other `kube-env` entries, such as `SECONDARY_BOOT_DISKS` and `SERVER_BINARY_TAR_URL`, so this adds no new kind of dependency. A unit test pins the entries, and a GKE change surfaces as a node without `hugepages-2Mi` capacity, not as a wrong allocation. |
| Option A: the cache key misses `LinuxNodeConfig`             | Low        | Two classes with different page counts share instance type capacity      | A unit test lists instance types for two classes that differ only in `hugepageSize2m` and checks both capacities.                                                                                                                                                                                                                                                                                         |
| Option B: the overlay capacity does not match the page count | Medium     | The simulation launches a node that cannot fit the pod                   | Document that the overlay capacity is `hugepageSize2m * 2Mi`. Tools that generate both objects can compute one from the other.                                                                                                                                                                                                                                                                            |
| The page count is larger than the machine memory             | Low        | The node boots with fewer pages than requested, or fails to become ready | The same limit applies to GKE node pools. NodePool requirements restrict the class to machine types that fit.                                                                                                                                                                                                                                                                                             |

---

## Test Plan

### Unit Tests

- `buildInstance` with `hugepageSize2m: 4301` writes `HUGEPAGE_2M: "4301"` and `ENABLE_CONTAINERD_HUGETLB_CONTROLLER: "true"` to `kube-env`.
- `buildInstance` with no `linuxNodeConfig` removes a `HUGEPAGE_2M` entry inherited from the source node pool.
- The CRD rejects `hugepageSize2m: 0`.
- Option A: `computeCapacity` with `hugepageSize2m: 4096` has `hugepages-2Mi: 8Gi`, and the instance type allocatable memory is 8 GiB lower than without the field.
- Option A: `List` for two classes that differ only in `hugepageSize2m` returns different `hugepages-2Mi` capacity.

### E2E / Integration Tests

- Launch a node from a class with `hugepageSize2m: 512`. The node reports `hugepages-2Mi: 1Gi` capacity, and a pod that requests `hugepages-2Mi: 1Gi` runs on it. With Option A the test creates no `NodeOverlay`.

---

## Alternatives Considered

### Custom metadata and a DaemonSet

Pass the page count in `spec.metadata` and allocate the pages from a DaemonSet that reads the metadata server. This needs no provider change, but the DaemonSet runs after the kubelet starts. The kubelet does not report hugepages allocated after startup, so the DaemonSet must restart the kubelet and gate scheduling with a startup taint. The GKE bootstrap already allocates the pages before the kubelet starts.

### A generic sysctl field

Add `linuxNodeConfig.sysctls` and set `vm.nr_hugepages`. GKE exposes hugepages as a dedicated field and not as a sysctl, and a sysctl does not enable the containerd hugetlb controller. A dedicated field matches the GKE API and states the intent.

---

## Future Direction

- `hugepageSize1g` in `HugepagesConfig`, which maps to the GKE `HUGEPAGE_1G` entry.
- Other `linuxNodeConfig` fields that GKE supports, such as `sysctls`, `cgroupMode`, and `transparentHugepageEnabled`.

---

## Open Questions

1. **Option A or Option B for scheduling capacity?** See [Scheduling Capacity](#scheduling-capacity). Open.
2. **Remove `ENABLE_CONTAINERD_HUGETLB_CONTROLLER` when the field is not set?** This proposal removes only `HUGEPAGE_2M`. An enabled hugetlb controller without hugepages has no effect, and a source node pool may set it for other reasons. Open.
3. **Include `hugepageSize1g` now?** It adds one field and one `kube-env` entry, but this proposal has no user for it yet. Open.

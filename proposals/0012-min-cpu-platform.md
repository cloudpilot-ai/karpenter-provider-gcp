# Proposal: Minimum CPU platform on GCENodeClass

- **Status**: Provisional
- **Authors**: @charlie-e2b
- **Created**: 2026-11-02
- **Related Issues**: none

---

## Summary

Add an optional `spec.minCpuPlatform` string to `GCENodeClass`, passed through to the GCE instance `minCpuPlatform` field on create. GKE node pools expose this as `min_cpu_platform`; Karpenter-provisioned nodes currently have no way to set it, so a machine family that GCE offers on several CPU generations can return a different CPU on each launch.

The field is one string, hashed into the existing NodeClass drift hash so changing it rolls nodes like every other NodeClass field.

---

## Motivation

### Problem Statement

Several machine families map to more than one CPU platform: N2 and N2D can land on two generations, and E2 makes no platform promise at all. GKE node pools pin this with `min_cpu_platform`. Teams migrating such a pool to Karpenter lose the pin, because `GCENodeClass` has no equivalent and `pkg/providers/instance` never sets `MinCpuPlatform` on the `compute.Instance` it builds.

For most workloads this is harmless. For workloads that depend on an exact CPU feature set it is not: a Firecracker or other microVM snapshot taken on an Ice Lake node fails to restore on a Cascade Lake node, so a pool where nodes are cycled by `expireAfter` slowly becomes a mix that refuses each other's snapshots.

### Goals

- Let a `GCENodeClass` pin the minimum CPU platform the same way a GKE node pool can.
- Treat a change to the pin as NodeClass drift.

### Non-Goals

- Validating the platform name against the machine type; GCE rejects an unsupported combination at create and the error already surfaces on the `NodeClaim` `Launched` condition, as it does for `confidentialInstanceType`.
- Exposing CPU platform as a scheduling label or requirement.

---

## Proposal

### Overview

```yaml
apiVersion: karpenter.k8s.gcp/v1alpha1
kind: GCENodeClass
spec:
  minCpuPlatform: "Intel Ice Lake"
```

### Design Details

- `GCENodeClassSpec.MinCPUPlatform *string` with JSON tag `minCpuPlatform`, `MinLength=1`, `MaxLength=64`, optional. The value is the GCE platform name, for example `Intel Ice Lake` or `AMD Milan`.
- `buildInstance` copies the value to `instance.MinCpuPlatform` when set. Nothing else on the instance changes; GCE handles "this platform or newer from the same vendor" semantics.
- `GCENodeClass.Hash()` already hashes the whole spec, so the field participates in drift with no extra code. A drift test case covers it.
- Unset keeps today's behaviour exactly.

---

## Acceptance Criteria

- A `GCENodeClass` with `minCpuPlatform` set produces instances whose `minCpuPlatform` matches (`gcloud compute instances describe`).
- Changing the value marks existing nodes drifted.
- A `GCENodeClass` without the field is unchanged.

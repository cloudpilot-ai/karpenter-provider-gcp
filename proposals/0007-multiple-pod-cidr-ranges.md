# Proposal: Multiple pod CIDR ranges on GCENodeClass

- **Status**: Implementable
- **Authors**: @guyeisenbach
- **Created**: 2026-08-20
- **Related Issues**: [#572](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/572)

---

## Summary

`GCENodeClass` currently exposes a single optional `spec.subnetRangeName` for the GKE secondary IPv4 range used as pod alias IPs. Operators with [additional pod ranges](https://cloud.google.com/kubernetes-engine/docs/how-to/multi-pod-cidr) must clone NodeClass and NodePool objects to spill over when the default range is exhausted.

This proposal adds `spec.subnetRangeNames`, an optional list of secondary range names. At launch the provider ranks those ranges by GKE-reported utilization and allocates from the least-used range. `spec.subnetRangeName` remains for backward compatibility and is mutually exclusive with the list.

---

## Motivation

### Problem Statement

Unset `subnetRangeName` always uses `IpAllocationPolicy.ClusterSecondaryRangeName`. It does not consult `additionalPodRangesConfig`. Pinning a NodeClass to one extra range requires a full copy of every NodeClass and NodePool, with `weight` offsets so Karpenter does not always pick the same copy.

AWS Karpenter ranks matching subnets by available IPs. GCP secondary ranges are named resources, so a list of range names plus launch-time selection is the close analog.

### Goals

- Let one GCENodeClass name several GKE pod secondary ranges.
- Pick the range with the most remaining capacity at launch (lowest GKE utilization).
- Keep existing `subnetRangeName` YAML working.
- Surface resolved range utilization on NodeClass status.
- Retry remaining listed ranges when Compute returns IP space exhausted.

### Non-Goals

- Automatically spilling into `additionalPodRangesConfig` when neither field is set (preserves today's default).
- AWS-style tag selectors for secondary ranges.
- Changing which GKE cluster pod ranges exist (operators still add ranges on the cluster).

---

## Proposal

### Overview

Keep `spec.subnetRangeName`. Add `spec.subnetRangeNames`. CEL rejects setting both. Launch resolves candidates, ranks by GKE utilization, sets `AliasIpRanges[0].SubnetworkRangeName`, and retries other candidates on `IP_SPACE_EXHAUSTED`.

### Design Details

#### API

```yaml
spec:
  subnetRangeNames:
    - gke-example-pods
    - gke-example-additional
```

- Per-item validation matches `subnetRangeName` (RFC 1035-ish GCE range name).
- `MinItems=1`, `MaxItems=16`, unique items.
- Unset list and unset single field: cluster default only (no auto-include of additional ranges).

Helper `GCENodeClass.PodSubnetRangeNames()` returns the list, else a one-element slice from `subnetRangeName`, else nil.

#### Capacity observation

GKE already reports utilization on the cluster object:

- Default range: `IPAllocationPolicy.DefaultPodIpv4RangeUtilization`
- Additional ranges: `IPAllocationPolicy.AdditionalPodRangesConfig.PodRangeInfo[].Utilization`

Rank listed names by lowest known utilization. Names without utilization sort after known values, preserving spec order as a tie-breaker.

`GetClusterConfig` is cached for 30 minutes, so ranking is best-effort. Insert retry covers stale utilization.

#### Launch and IP exhaustion

Today a single `IP_SPACE_EXHAUSTED` / `IP_SPACE_EXHAUSTED_WITH_DETAILS` fails fast and marks the zone offering unavailable (other instance types share the subnet). With multiple candidates, retry Insert with the next range **before** marking IP space exhausted. Fail-fast only after every candidate fails.

Which range is chosen among a list is not hashed. Changing the list itself is hashed (`GCENodeClassHashVersion` stays `v4`; the new field is optional and `IgnoreZeroValue` leaves existing hashes unchanged).

#### Status

`status.subnetRanges` lists each resolved candidate `name` and optional `utilization`, analogous to AWS `status.subnets`. A nodeclass status reconciler fills this from `GetClusterConfig`.

#### Drift

Changing `subnetRangeName` or `subnetRangeNames` is NodeClass drift. Launch-time choice among listed ranges is not drift.

---

## Risks and Mitigations

| Risk                                               | Likelihood | Impact                                | Mitigation                                                                    |
|----------------------------------------------------|------------|---------------------------------------|-------------------------------------------------------------------------------|
| Stale cluster-cache utilization                    | Medium     | Temporary preference for a full range | Retry remaining ranges on IP_SPACE_EXHAUSTED                                  |
| Operator lists a range not attached to the cluster | Low        | Insert failure                        | Document that names must be the default or `additionalPodRangesConfig` ranges |
| Mutual-exclusivity surprise                        | Low        | CRD reject                            | Docs + CEL message; keep single-field path                                    |

---

## Test Plan

### Unit Tests

- CRD CEL: both fields set is rejected; list item pattern; unique items.
- Ranking: lowest utilization first; unknown last; spec-order tie-break.
- Launch: `subnetRangeName` still overrides cluster default; list selects least-used range.
- Insert: IP_SPACE_EXHAUSTED retries the next range and only fail-fasts after the last.
- Drift: changing `subnetRangeNames` is NodeClass drift.

### E2E / Integration Tests

Existing e2e NodeClasses may keep `subnetRangeName`. Multi-range spillover is unit-tested; live additional-range clusters are optional follow-up.

---

## Acceptance Criteria

The feature is complete when:

- [ ] `spec.subnetRangeNames` is on the CRD and mutually exclusive with `subnetRangeName`
- [ ] Launch ranks by GKE utilization and retries on IP space exhaustion
- [ ] `status.subnetRanges` is populated
- [ ] Docs and examples cover the list field
- [ ] Existing `subnetRangeName` configs keep working

---

## Migration

Additive. No required operator action. To spill over across additional pod ranges, list those names (including the cluster default) on `subnetRangeNames` and remove `subnetRangeName` if it was set.

---

## Alternatives Considered

### Drop `subnetRangeName` and migrate to a list

Would force a CRD/YAML migration for every existing NodeClass. Keeping the scalar field is cheaper while v1alpha1 is still in motion.

### Auto-include `additionalPodRangesConfig` when unset

Would change default launch behavior and could allocate from ranges operators did not intend. Opt-in via the list is explicit.

### Count remaining alias IPs via Compute instance listing

More accurate than GKE utilization but expensive and racy. GKE utilization plus Insert retry is sufficient.

---

## Future Direction

Shorter cluster-config TTL specifically for utilization, or a dedicated pod-range provider cache similar to AWS subnets.

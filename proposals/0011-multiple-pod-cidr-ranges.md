# Proposal: Multiple pod CIDR ranges on GCENodeClass

- **Status**: In Review (implemented by [#573](https://github.com/cloudpilot-ai/karpenter-provider-gcp/pull/573))
- **Authors**: @guyeisenbach
- **Created**: 2026-08-20
- **Related Issues**: [#572](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/572)

---

## Summary

Before this change, `GCENodeClass` exposed a single optional `spec.subnetRangeName` for the GKE secondary IPv4 range used as pod alias IPs. Operators with [additional pod ranges](https://cloud.google.com/kubernetes-engine/docs/how-to/multi-pod-cidr) must clone NodeClass and NodePool objects to spill over when the default range is exhausted.

This proposal adds `spec.subnetRangeNames`, an optional list of secondary range names. At launch the provider ranks those ranges by Compute-reported free IPv4 addresses and prefers the range with the greatest known count. `spec.subnetRangeName` is deprecated but remains for backward compatibility and is mutually exclusive with the list. If both fields are omitted, candidates are discovered from the cluster's default and additional pod ranges on its primary subnetwork. Either explicit field replaces the discovered list completely.

---

## Motivation

### Problem Statement

Before this change, unset `subnetRangeName` used only `IpAllocationPolicy.ClusterSecondaryRangeName` without consulting `additionalPodRangesConfig`. Pinning a NodeClass to one extra range requires a full copy of every NodeClass and NodePool, with `weight` offsets so Karpenter does not always pick the same copy.

AWS Karpenter ranks matching subnets by available IPs. GCP secondary ranges are named resources, so a list of range names plus launch-time selection is the close analog.

### Goals

- Let one GCENodeClass name several GKE pod secondary ranges.
- Discover cluster-level pod ranges by default, with complete replacement by explicit `subnetRangeName` or `subnetRangeNames` settings.
- Prefer the range with the greatest known free IPv4 address count at launch.
- Keep existing `subnetRangeName` YAML working.
- Surface resolved range free-IP counts on NodeClass status.
- Retry remaining listed ranges when Compute returns IP space exhausted.

### Non-Goals

- AWS-style tag selectors for secondary ranges.
- Changing which GKE cluster pod ranges exist (operators still add ranges on the cluster).
- Reproducing GKE's node-pool pod-range allocation algorithm.
- Discovering ranges on separate subnetworks (`additionalIpRangesConfigs`); only the cluster's primary subnetwork is considered.

---

## Proposal

### Overview

Keep `spec.subnetRangeName`. Add `spec.subnetRangeNames`. CEL rejects setting both. Launch resolves candidates, ranks by Compute free-IP counts, sets `AliasIpRanges[0].SubnetworkRangeName`, and retries other candidates on `IP_SPACE_EXHAUSTED`.

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
- Unset list and unset single field: discover the cluster default and additional pod ranges on the primary subnetwork.
- Explicit `subnetRangeName` or `subnetRangeNames`: replace the discovered list entirely, without merging cluster ranges.
- Discovery combines the primary range, `additionalPodRangesConfig.podRangeNames`, and names from `podRangeInfo` in that order, removing duplicates and empty names. Separate-subnetwork `additionalIpRangesConfigs` are excluded.

Helper `GCENodeClass.PodSubnetRangeNames()` returns the list, else a one-element slice from `subnetRangeName`, else nil. Launch and status use the same cluster-range discovery helper when it returns nil. If no named range is reported, launch retains one attempt with the range name unset.

#### Capacity observation

Read the effective primary subnetwork through `subnetworks.get` with `views=WITH_UTILIZATION`. Each named entry in `utilizationDetails.ipv4Utilizations` supplies an optional `totalFreeIp` count; the unnamed primary IPv4 range is excluded. Fully qualified references identify the target project and region, including Shared VPC host projects. Bare references require a fully qualified effective network to resolve ownership safely.

Rank resolved names by greatest known free-IP count. Unknown counts sort after known values, preserving candidate order as a tie-breaker. Known zero counts remain eligible because snapshots do not guarantee whether a contiguous pod CIDR block can be allocated.

A shared subnet provider caches successful snapshots for one minute. Capacity reads have a three-second deadline and occur once per `Create`, not once per range or instance type. Failed reads are logged and launch continues in candidate order. Status publishes the resolved names without counts and still requeues every five minutes.

`GetClusterConfig` is cached for 30 minutes, so eligibility discovery remains best-effort. Discovery follows GKE's cluster-level range configuration; selection uses this provider's free-IP heuristic rather than reproducing GKE's node-pool allocation algorithm. Insert retry remains authoritative.

#### Launch and IP exhaustion

Today a single `IP_SPACE_EXHAUSTED` / `IP_SPACE_EXHAUSTED_WITH_DETAILS` fails fast and marks the zone offering unavailable (other instance types share the subnet). With multiple candidates, retry Insert with the next range **before** marking IP space exhausted. Fail-fast only after every candidate fails.

Which range is chosen among a list is not hashed. Changing the list itself is hashed. This change does not bump `GCENodeClassHashVersion`; the new field is optional and `IgnoreZeroValue` leaves existing hashes unchanged.

#### Status

`status.subnetRanges` lists each resolved candidate `name` and optional integer `totalFreeIP`, analogous to AWS subnet capacity observations. An omitted count is unknown; an explicit zero is preserved. The reconciler combines GKE eligibility with counts from the shared subnet provider. Capacity failures do not change image readiness.

#### Drift

Changing `subnetRangeName` or `subnetRangeNames` is NodeClass drift. Launch-time choice among listed ranges is not drift.

---

## Risks and Mitigations

| Risk                                               | Likelihood | Impact                                | Mitigation                                                                    |
|----------------------------------------------------|------------|---------------------------------------|-------------------------------------------------------------------------------|
| Stale subnet capacity snapshot                     | Medium     | Temporary preference for a full range | Retry remaining ranges on IP_SPACE_EXHAUSTED                                  |
| Operator lists a range not attached to the cluster | Low        | Insert failure                        | Document that names must be the default or `additionalPodRangesConfig` ranges |
| Mutual-exclusivity surprise                        | Low        | CRD reject                            | Docs + CEL message; keep single-field path                                    |

---

## Test Plan

### Unit Tests

- CRD CEL: both fields set is rejected; list item pattern; unique items.
- Ranking: greatest free-IP count first; unknown last; candidate-order tie-break; known zero remains eligible.
- Discovery: default plus configured and reported additional names; stable union, deduplication, missing utilization, missing primary name, and exclusion of separate subnetworks.
- Launch: omitted fields discover candidates; explicit `subnetRangeName` or `subnetRangeNames` settings completely replace them; greatest known free-IP count is preferred; capacity read failure does not block launch.
- Status: candidate membership matches launch, with optional integer free-IP counts; clear stale counts on failure without a reconcile error.
- Capacity: HTTP query/view, omitted-versus-zero fields, Shared VPC/override resolution, cache identity/expiry/ownership and bounded cancellation.
- Insert: IP_SPACE_EXHAUSTED retries the next range and only fail-fasts after the last.
- Drift: changing `subnetRangeNames` is NodeClass drift.

### E2E / Integration Tests

Existing e2e NodeClasses may keep `subnetRangeName`. E2e setup provisions two dedicated pod ranges on the primary subnet, `-pods-small` (/22) and `-pods-large` (/20), attaches them to the cluster, and passes their names to the suite. Serial networking specs cover default discovery of the cluster's ranges, `subnetRangeName` and `subnetRangeNames` overrides asserted against the node's actual primary-interface alias range, published free-IP counts compared with a direct `subnetworks.get`, and placement in the range with the most free IPs when the poorer range is listed first.

---

## Acceptance Criteria

The feature is complete when:

- [X] `spec.subnetRangeNames` is on the CRD and mutually exclusive with `subnetRangeName`
- [X] Launch ranks by Compute free-IP counts and retries on IP space exhaustion
- [X] `status.subnetRanges` is populated
- [X] Docs and examples cover the list field
- [X] Existing `subnetRangeName` configs keep working

---

## Migration

Omitted pod-range fields now allow allocation from the cluster default and additional pod ranges. To retain default-range-only allocation, explicitly set `subnetRangeNames` to a single-element list containing the cluster default range name. Explicit `subnetRangeName` or `subnetRangeNames` settings restrict allocation to the specified names. Migrate deprecated `subnetRangeName` to a single-element `subnetRangeNames` list; removal will be announced separately. Status reports optional integer `totalFreeIP` counts. The existing `compute.subnetworks.get` permission is now used for capacity reads, including in Shared VPC host projects.

---

## Alternatives Considered

### Drop `subnetRangeName` and migrate to a list

Would force a CRD/YAML migration for every existing NodeClass. Keeping `subnetRangeName` avoids that migration while v1alpha1 is still in motion.

### Use only the cluster default range when unset

This preserves the previous default but does not follow GKE's cluster-level additional pod range configuration. Automatic discovery is used instead; operators can restrict allocation with an explicit list.

### Count remaining alias IPs via Compute instance listing

Unnecessary and expensive: `subnetworks.get` already provides per-range free-IP counts. Those snapshots plus Insert retry avoid reconstructing allocation state from instances.

### Rank by GKE cluster utilization

A relative ratio can prefer a small range over a larger range with more free addresses, and describes cluster-level usage rather than subnet-wide free capacity. Compute free-IP counts are a more useful selection hint.

---

## Future Direction

Shorter cluster-config TTL for eligibility discovery, or in-flight allocation tracking to reduce concurrent selection of the same range. Compute insertion remains the authoritative capacity check.

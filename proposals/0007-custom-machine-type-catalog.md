# Proposal: GCE Custom Machine Type Catalog Registration

- **Status**: Draft
- **Authors**: @geekette86
- **Created**: 2026-09-26
- **Related Issues**: [#144](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/144)
- **Related PRs**: [#601](https://github.com/cloudpilot-ai/karpenter-provider-gcp/pull/601)

<!-- Supersedes the request-time resolution mechanism in PR #601 per maintainer review; see Alternatives Considered. -->

---

## Summary

GCE custom machine types (e.g. `n2-custom-8-24576`) are never returned by the `machineTypes.aggregatedList` API that populates Karpenter's instance type catalog, because GCP does not enumerate the space of valid custom shapes. This proposal adds a cluster-scoped CRD, `GCECustomMachineType`, that lets an operator explicitly register a custom shape so it joins the instance type catalog as a first-class member: selectable by `node.kubernetes.io/instance-type` exactly as today, but also by ordinary CPU/memory-style requirements, and with an explicit, operator-supplied price rather than one inferred from predefined shapes.

The provider resolves a registered shape's vCPU/memory and per-zone availability from `machineTypes.get` (which does support custom shapes, unlike the aggregated list) via a dedicated reconciler, not on every scheduling pass. Phase 1 requires the operator to supply on-demand and Spot prices on the CRD, since GCP's real custom-shape pricing includes a premium and a separate extended-memory (`-ext`) rate that a linear fit over predefined shapes cannot reproduce exactly, and this price drives scheduling and consolidation decisions, not just cost reporting. Phase 2, gated on [#218](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/218), teaches the pricing provider GCP's actual custom-rate formula so supplying a price becomes an optional override instead of a requirement.

---

## Motivation

### Problem Statement

PR #601's current approach resolves a custom machine type only when a NodePool's `node.kubernetes.io/instance-type` requirement names the exact shape with an `In` operator, at the moment `List()` is called for scheduling. This has two problems raised in review:

1. **Requirements-based selection doesn't work.** A NodePool that constrains by CPU/memory instead of an exact instance-type name (the normal Karpenter pattern) will never see a custom shape, because it's absent from `instanceTypesInfo` until something explicitly asks for its exact name.
2. **Pricing is inferred, not authoritative.** The current `deriveCustomPrice` fits a line across predefined sibling shapes' known prices to estimate a custom shape's price. This is a reasonable approximation but not what GCP actually bills: custom shapes carry their own premium over predefined rates, and the `-ext` (extended memory) variant bills memory at a different rate again. Since Karpenter uses price to rank and consolidate instance types, a wrong price is a scheduling-correctness problem, not just a cosmetic one.

### Goals

- Let a registered custom machine type appear in the instance type catalog like any predefined type: selectable by exact name or by CPU/memory requirements.
- Require an explicit, operator-supplied price for a registered type in phase 1, rather than an inferred one.
- Resolve a registered type's vCPU/memory/zone-availability via `machineTypes.get`, reusing the lookup and per-zone caching already built in PR #601, but on a reconcile loop rather than inline in the scheduling hot path.
- Keep `GCENodeClass` scoped to node configuration; machine type definition lives on the new CRD.
- Set up phase 2 (accurate computed custom pricing) as a additive, non-breaking follow-on once #218 lands.

### Non-Goals

- Computing GCP's real custom-shape pricing formula in phase 1. That is phase 2, explicitly gated on #218.
- Supporting an unregistered custom shape purely by naming it in a NodePool requirement. Phase 1 requires registration first; this is a behavior change from PR #601's current (unmerged) mechanism.
- Auto-suggesting or generating custom shapes on the operator's behalf.

---

## Proposal

### Overview

Before (PR #601, current state):

```text
NodePool requirement names "n2-custom-8-24576" exactly
  -> List() calls machineTypes.get per zone, on every scheduling pass
  -> price estimated from predefined sibling shapes
  -> instance type returned only for that one List() call, to that one caller
```

After (this proposal):

```yaml
apiVersion: karpenter.k8s.gcp/v1alpha1
kind: GCECustomMachineType
metadata:
  name: n2-custom-8-24576
spec:
  machineType: n2-custom-8-24576
  prices:
    onDemand: "0.40"
    spot: "0.12"
```

```text
GCECustomMachineType created
  -> controller resolves vCPU/memory/zones via machineTypes.get, writes .status
  -> Ready condition true
  -> instancetype provider merges it into the regular catalog (instanceTypesInfo)
  -> selectable by node.kubernetes.io/instance-type, OR by
     karpenter.k8s.gcp/instance-cpu / instance-memory requirements, like any
     predefined shape
```

### API Changes

#### `GCECustomMachineType` (new, cluster-scoped CRD)

```yaml
apiVersion: karpenter.k8s.gcp/v1alpha1
kind: GCECustomMachineType
metadata:
  name: n2-custom-8-24576
spec:
  # Must match <family>-custom-<vCPUs>-<memoryMB>(-ext)?. Immutable after creation.
  machineType: n2-custom-8-24576
  prices:
    onDemand: "0.40"  # required in phase 1; optional override in phase 2
    spot: "0.12"      # required in phase 1; optional override in phase 2
status:
  conditions:
    - type: Ready
      status: "True"
  guestCpus: 8
  memoryMb: 24576
  zones: ["us-central1-a", "us-central1-b"]
```

`spec.machineType` is validated against the same pattern PR #601 already uses (`^[a-z][a-z0-9]*-custom-[0-9]+-[0-9]+(-ext)?$`), via CEL/admission validation, and is immutable (use a new object to rename). The resource name defaults to `spec.machineType` by convention but isn't required to match it.

`spec.prices` fields are decimal strings (matching how the rest of the codebase represents currency, consistent with `pricing.Provider`'s `float64` internally but avoiding float precision issues at the API boundary). Both are required in phase 1.

This is a sketch, not a final schema — see Open Questions.

#### New controller: `gcecustommachinetype`

A new reconciler (alongside the existing `instancetype` catalog-refresh controller in `pkg/controllers/providers/instancetype`) watches `GCECustomMachineType` objects:

- Resolves `spec.machineType` via `machineTypes.get` per cluster zone (reusing PR #601's `getCustomMachineType`/caching logic, moved here).
- Writes `status.guestCpus`, `status.memoryMb`, `status.zones`, and the `Ready` condition.
- Re-resolves periodically (same cadence as the existing instance-type catalog refresh) to pick up new zone availability, and re-validates that the shape is still resolvable.
- A shape that fails to resolve in every zone sets `Ready=False` with a reason; it does not appear in the catalog.

#### Instance type provider changes

- `instancetype.DefaultProvider` gains a source for `GCECustomMachineType` objects (via the existing `kubeClient`, or a passed-in lister) and merges `Ready` ones into `instanceTypesInfo`/`instanceTypesOfferings` during its regular refresh, exactly like aggregated-list results.
- Pricing for a catalog entry backed by a `GCECustomMachineType` comes from `spec.prices` directly — no fallback to `deriveCustomPrice`. If a registered type is missing a price in phase 1, it does not get an offering (fails validation at admission instead, ideally, per Acceptance Criteria).
- PR #601's request-time resolution path (`resolveCustomInstanceTypes`, `getCachedCustomMachineType` invoked from `List()`, `deriveCustomPrice`) is removed. The `machineTypes.get` call and its per-name/zone caching move into the new controller instead.

### Design Details

Data flow:

```text
GCECustomMachineType (spec.machineType, spec.prices)
  -> gcecustommachinetype controller
       -> machineTypes.get(zone, spec.machineType) per cluster zone
       -> status.{guestCpus, memoryMb, zones, Ready}
  -> instancetype.DefaultProvider.UpdateInstanceTypes / UpdateInstanceTypeOfferings
       -> merges Ready GCECustomMachineType objects into instanceTypesInfo
  -> instancetype.DefaultProvider.List()
       -> same computeRequirements/createOfferings path as predefined types
       -> price read from spec.prices, not derived
```

Key invariant carried over from PR #601: `karpenter.sh/instance-type` always equals the real GCE machine type name (`spec.machineType`), so `node.kubernetes.io/instance-type: n2-custom-8-24576` in a NodePool continues to work unchanged from a user's perspective — the mechanism behind it moves from request-time resolution to catalog membership.

---

## Risks and Mitigations

| Risk | Likelihood | Impact | Mitigation |
|------|------------|--------|------------|
| Operator supplies a stale or wrong price | Medium | Scheduling/consolidation decisions based on wrong cost | Phase 2 computes the authoritative price; until then, document the risk and consider a webhook warning if the supplied price deviates far from a sanity-check estimate |
| `machineType` doesn't actually exist for the family/zone | Low | Registered type never resolves, `Ready=False`, silently unschedulable | Surface `Ready=False` reason clearly (`kubectl describe gcecustommachinetype`); consider an event/metric |
| Removing PR #601's request-time resolution breaks anyone who already adopted it | Low (unreleased) | None — #601 is unmerged | No migration needed; call this out explicitly in the PR |
| CRD proliferation for large custom fleets | Low | Operational overhead of one object per shape | Out of scope for phase 1; could add a bulk/templated registration mechanism later if needed |

---

## Test Plan

### Unit Tests

- `GCECustomMachineType` admission validation: `machineType` pattern, immutability, required `prices` fields in phase 1.
- Controller reconciliation: resolves CPU/memory/zones via a fake `machineTypes.get`; sets `Ready` correctly on success, partial-zone success, and total failure; leaves a zone unresolved (not cached as unavailable) on a transient (non-404) error, consistent with the error-handling fix already in PR #601.
- Instance type provider: a `Ready` `GCECustomMachineType` appears in `List()` output with the CRD's price, matched by both instance-type and CPU/memory requirements; a non-`Ready` one does not appear.

### E2E / Integration Tests

- Register a `GCECustomMachineType`, create a NodePool selecting it via `node.kubernetes.io/instance-type`, confirm a node provisions, joins the cluster, and a workload schedules and runs on it.
- Same, but the NodePool selects by CPU/memory requirements instead of exact instance type name, with no other constraint pointing at the custom shape.

---

## Acceptance Criteria

The feature is complete when:

- [ ] `GCECustomMachineType` CRD is defined, validated, and documented.
- [ ] A dedicated controller resolves registered shapes via `machineTypes.get` and maintains `status`.
- [ ] `instancetype.DefaultProvider` merges `Ready` registrations into the regular catalog.
- [ ] A registered custom shape is schedulable via `node.kubernetes.io/instance-type` and via CPU/memory requirements.
- [ ] Pricing comes from `spec.prices` only; `deriveCustomPrice` and PR #601's request-time resolution path are removed.
- [ ] Unit and e2e coverage per Test Plan passes.
- [ ] `docs/troubleshooting.md`'s custom-machine-type section is rewritten for the new mechanism.

---

## Implementation Phases

### Phase 1 — Explicit registration, required user-supplied prices

As described above. `spec.prices.onDemand`/`spec.prices.spot` are required.

### Phase 2 — Computed custom pricing (gated on #218)

Once #218 lands, the pricing provider learns GCP's actual custom-shape pricing formula (per-vCPU/per-GB rate plus the documented custom-shape premium, with the separate extended-memory rate applied when `machineType` ends in `-ext`). `spec.prices` becomes optional: when omitted, the computed price is used; when present, it remains an explicit override. No change to `spec.machineType` or the catalog-membership mechanism is required for this phase.

---

## Migration

None required. PR #601's mechanism has not been released, so there is no deployed behavior to migrate away from — this proposal replaces it before merge rather than deprecating it after.

---

## Alternatives Considered

### Request-time resolution keyed off an exact NodePool requirement (PR #601, as submitted)

Resolves a custom shape only when a NodePool's `instance-type` requirement names it exactly, doing the `machineTypes.get` lookup (with caching) inline in `List()`, and estimating its price from predefined sibling shapes via a least-squares fit.

Rejected as the long-term mechanism because:
- It cannot support CPU/memory-based requirements, only exact-name pinning.
- Estimated pricing can misprice scheduling/consolidation decisions; GCP's real custom rates include a premium and a distinct extended-memory rate this fit does not model.
- The lookup and caching live in the scheduling hot path (`List()`) rather than a natural periodic-refresh reconciler, mirroring the pattern already used for the predefined catalog.

Its `machineTypes.get`-based resolution and per-zone caching are still useful and are reused inside the new controller.

---

## Future Direction

- Bulk/templated registration for operators managing many custom shapes (e.g. a range of vCPU/memory combinations for one family) if the one-CRD-per-shape model proves operationally heavy.
- Surfacing a price-sanity warning (event or status condition) when a phase 1 user-supplied price deviates significantly from what phase 2's formula would compute, once both exist side by side.

---

## Open Questions

1. **Is `GCECustomMachineType` cluster-scoped or namespaced?** Sketch above assumes cluster-scoped, matching `GCENodeClass`. Open.
2. **Does admission validate `spec.machineType` against a live `machineTypes.get` call, or only against the name pattern, leaving live resolution to the controller?** Sketch above assumes pattern-only at admission (avoids admission-time GCP API calls); the controller's `status.conditions[Ready]` is the source of truth for actual availability. Open.
3. **Which labels express CPU/memory requirements for a registered type — the existing `karpenter.k8s.gcp/instance-cpu` / `instance-memory` (shared with predefined types), or custom-type-specific ones?** Sketch above assumes the existing shared labels, computed from `status.guestCpus`/`status.memoryMb`, so a requirement written against predefined types also matches a registered custom type with the same CPU/memory. Open.
4. **Should `spec.prices.spot` be optional in phase 1, defaulting to the existing `pricing.SpotFallbackRatio` (40% of on-demand) the way predefined types without a published spot price already work?** The maintainer's sketch shows both as required; open whether the existing fallback ratio is an acceptable phase 1 default instead of a hard requirement.

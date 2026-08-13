# Proposal: Spot Preemption Notice

- **Status**: Provisional
- **Authors**: @ryan-macdonald-nb
- **Created**: 2026-08-13
- **Related Issues**: [#295](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/295)

---

## Summary

Karpenter-GCP currently learns that a Spot VM is going away only after GCE has already sent the ACPI G2 Soft Off signal. The interruption controller detects this by watching for a node whose `NodeReady` condition reports `KubeletNotReady` with the message `node is shutting down`. Kubelet only writes that after the signal arrives, so several seconds of the 30 second shutdown window are gone before Karpenter starts draining.

GCE exposes `scheduling.preemptionNoticeDuration`. Set to 120 seconds, the `instance/preempted` metadata key flips two minutes before the shutdown signal. That key is per-instance metadata, readable only from the instance itself, so reading it requires something running on the node.

This proposal adds three things: a `GCENodeClass.spec.preemptionNoticeDuration` field that requests the notice, an optional DaemonSet that watches the metadata key and sets a `GCESpotPreempting` node condition, and handling in the interruption controller that reacts to that condition. The existing kubelet path stays as the fallback.

---

## Motivation

### Problem Statement

`handleStoppingSpotInstances` in `pkg/controllers/interruption/controller.go` is the only detection path today:

```go
condition := node.GetCondition(&currentNode, corev1.NodeReady)
if condition.Status != corev1.ConditionTrue &&
	condition.Reason == NodeConditionReasonKubeletNotReady &&
	condition.Message == NodeConditionMessageShuttingDown {
	// delete the NodeClaim
}
```

This is reactive by construction. Kubelet has to notice the shutdown, update its own status, and have that reach the API server before Karpenter sees anything, and all of that happens inside the shutdown window rather than before it. AWS Karpenter gets a two-minute warning from an SQS interruption notice; GCP users get whatever is left of 30 seconds.

The path also passes `markUnavailable: false`, so a preempted offering is not taken out of rotation. `cleanNodeClaimByInstanceName` accepts the flag but no caller has ever set it to `true`.

### Goals

- Give Karpenter up to two minutes of warning before a Spot VM shuts down.
- Let operators request the notice per NodeClass.
- Keep the existing kubelet detection working as a fallback.
- Add nothing to existing installations that did not ask for it.

### Non-Goals

- Changing how draining itself works. This proposal only moves the trigger earlier; the drain is unchanged.
- Detecting on-demand instance termination. On-demand VMs are not preempted.
- Replacing the GKE-managed node-problem-detector or the conditions [node repair](../docs/node-repair.md) depends on.

---

## Proposal

### Overview

Before — one signal, arriving after shutdown has begun:

```
GCE sends G2 Soft Off ──▶ kubelet marks node shutting down ──▶ Karpenter drains
                          (already inside the 30s window)
```

After — with `preemptionNoticeDuration: 120` and the detector deployed:

```
GCE flips instance/preempted ──▶ detector sets GCESpotPreempting=True ──▶ Karpenter drains
        │                                                                      │
        └────────────────────── 120 seconds ───────────────────────────────────┘
                                                                               ▼
                                                        GCE sends G2 Soft Off ──▶ kubelet
                                                                        (fallback path)
```

### Design Details

**API.** A new field on `GCENodeClassSpec`:

```go
// +kubebuilder:validation:Enum=0;120
// +kubebuilder:default=0
// +optional
PreemptionNoticeDuration int64 `json:"preemptionNoticeDuration,omitempty"`
```

The enum matches what GCE currently accepts. `setupScheduling` in `pkg/providers/instance/instance.go` takes the NodeClass and, for Spot capacity with a non-zero value, sets `Scheduling.PreemptionNoticeDuration`. Zero is left unset rather than sent explicitly, since zero is already the GCE default.

The field participates in the NodeClass hash, so changing it drifts nodes. That is correct: GCE only accepts the setting at instance creation, so applying it requires replacement.

**Detection.** An optional DaemonSet running upstream `node-problem-detector` with a single custom-plugin monitor. The plugin reads `instance/preempted` once, then long-polls it with `wait_for_change=true`; a non-zero exit sets `GCESpotPreempting=True` on the node. Reading before waiting matters, because `wait_for_change` blocks until the value *changes* — a node already flagged would otherwise wait out the full timeout.

Two deployment details are load-bearing. The pod uses host networking, because pods otherwise reach the GKE metadata server, which does not expose `instance/preempted`. And it addresses `169.254.169.254` directly rather than resolving `metadata.google.internal`, so detection does not depend on cluster DNS during a shutdown.

**Reaction.** The interruption controller gains a `GCESpotPreempting` check, ordered ahead of the kubelet check since the notice arrives first. This path passes `markUnavailable: true`, the first real use of that parameter, so replacement capacity is not immediately requested from the zone and instance type GCE just reclaimed. A `PreemptionNoticeReceived` event is recorded against the Node.

### Two corrections to the issue

Issue #295 states that `preemptionNoticeDuration` "requires the `compute/beta` API and is currently in Preview", and defers the API field until it graduates. That is no longer accurate. The field is on the stable client this repo already vendors and uses — `Scheduling.PreemptionNoticeDuration` in `vendor/google.golang.org/api/compute/v1/compute-gen.go`, and `pkg/providers/instance/instance.go` imports `google.golang.org/api/compute/v1`. No beta package is vendored. The API field can ship now.

The issue also proposes `npd.enabled: true` by default. This proposal defaults it to `false` — see Risks below.

---

## Risks and Mitigations

| Risk                                                                                   | Likelihood           | Impact                              | Mitigation                                                                                                                         |
|----------------------------------------------------------------------------------------|----------------------|-------------------------------------|------------------------------------------------------------------------------------------------------------------------------------|
| A second node-problem-detector alongside the GKE-managed one                           | Certain when enabled | Extra agent per node                | Off by default; Spot-only node selector; distinct condition so the two never collide                                               |
| `preemptionNoticeDuration` rejected by GCE in some regions or on some machine families | Possible             | Instance creation fails             | Enum-restricted to `0` and `120`; zero is never sent explicitly, so existing behaviour is untouched for anyone who does not opt in |
| Detector misses a notice (crash, throttling, network)                                  | Low                  | Drain starts late                   | Kubelet fallback path is unchanged and still fires                                                                                 |
| Marking the offering unavailable shrinks the pool unnecessarily                        | Medium               | Fewer placement options for a while | See Open Questions                                                                                                                 |

---

## Test Plan

### Unit Tests

- `setupScheduling` sets the notice duration for Spot at `120`, leaves it nil at `0` and when unset, and never sets it for on-demand.
- The interruption controller deletes the NodeClaim, increments the disrupted counter, marks the offering unavailable, and publishes both events when a node reports `GCESpotPreempting=True`.
- The controller takes no action when the condition is present but `False`.

### Chart

- Rendering with defaults produces no DaemonSet; rendering with `spotPreemptionNotice.enabled=true` produces exactly one.
- The rendered monitor config parses as JSON and the rendered plugin passes `sh -n` and `shellcheck`.

### Not yet covered

The full 120 second path has not been exercised against a real preemption. GCE decides when to preempt, so this needs a live GKE cluster running Spot capacity, and confirmation that the condition appears roughly two minutes before shutdown. Manual verification steps are in [docs/spot-preemption.md](../docs/spot-preemption.md).

---

## Acceptance Criteria

- [ ] `spec.preemptionNoticeDuration` reaches the GCE instance and is visible via `gcloud compute instances describe`.
- [ ] `GCESpotPreempting=True` triggers NodeClaim deletion ahead of the kubelet condition.
- [ ] The offering is marked unavailable on preemption.
- [ ] Installations that do not opt in see no new workloads and no behaviour change.
- [ ] A real preemption on a node with `preemptionNoticeDuration: 120` produces the condition about two minutes before shutdown.

---

## Alternatives Considered

### node-problem-detector as a Helm subchart

This is what issue #295 proposes. Rejected for scope: the chart has no `dependencies:` block and no DaemonSet today, so this would introduce the first external chart-repo dependency, a `Chart.lock`, and a large third-party values surface that `values.schema.json` would have to declare in full — it sets `additionalProperties: false` at the root and in every nested object. Three hand-written templates give the same behaviour with a much smaller review and maintenance surface. If maintainers would rather take the subchart, the plugin config and condition semantics here carry over unchanged.

### A purpose-built Go agent instead of node-problem-detector

Rejected. It would mean owning a second binary, its release process, and its node-status patching. node-problem-detector already does condition management, and its custom-plugin model fits a long-polling check directly.

### Polling the GCE API from the controller

Rejected as unworkable. `instance/preempted` is per-instance metadata that only the instance can read. The controller could poll `instances.list` for status changes instead, but that is slow, expensive at scale, and still reports state after the fact rather than ahead of it.

---

## Future Direction

- If GCE accepts notice durations beyond 120 seconds, the enum widens with no other change.
- The unavailable-offerings TTL could be tuned per interruption cause rather than using the default.

---

## Open Questions

1. **Should the preemption path mark the offering unavailable?** Implemented as `true`, following the issue and matching the AWS provider. The counter-argument: GCP also preempts Spot VMs simply for reaching their 24 hour maximum lifetime, which is not a capacity signal, so treating every preemption as one may pull offerings out of rotation without cause. Open — maintainer call.
2. **Subchart or hand-written templates?** Proposal takes hand-written; see Alternatives. Open — maintainer call.
3. **Should `spotPreemptionNotice.enabled` default to `true`?** Proposal says no, because of the GKE-managed node-problem-detector already present on Standard node pools. Open.

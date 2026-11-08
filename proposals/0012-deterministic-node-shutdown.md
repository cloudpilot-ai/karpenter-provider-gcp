# Proposal: Deterministic node shutdown configuration

- **Status**: Draft
- **Authors**: @dm3ch; builds on @djeebus's [PR #732](https://github.com/cloudpilot-ai/karpenter-provider-gcp/pull/732)
- **Created**: 2026-11-08
- **Related Issues**: N/A

## Summary

Expose upstream kubelet `shutdownGracePeriod` and `shutdownGracePeriodCriticalPods` under `GCENodeClass.spec.kubeletConfiguration`, as proposed in #732. Use provider-owned defaults rather than copying shutdown policy from a bootstrap source pool.

Spot nodes retain 30s total and 15s for critical pods. On-demand nodes use 0s/0s unless explicitly configured. The latter matches upstream kubelet defaults and an observed GKE 1.35 on-demand configuration; it is not a claim of universal GKE parity.

## Motivation

Source pools provide bootstrap material, but their shutdown settings should not determine the policy of unrelated Karpenter nodes. Source selection can otherwise change shutdown behavior without a NodeClass change. Operators also need to enable graceful shutdown explicitly for on-demand nodes.

Duration validation must reject values that the controller cannot decode, including syntactically valid durations exceeding Go's range.

## Proposal

Before applying NodeClass kubelet settings, reset shutdown durations to 0s, clear source priority-based shutdown periods and remove the source `GracefulNodeShutdown` gate override. For Spot, set the gate and 30s/15s defaults. Explicit NodeClass total duration enables the gate and overrides the total; an explicit critical-pods duration overrides its default. Clamp an omitted critical-pods default to a shorter explicitly configured total, including zero.

Use upstream duration-valued field names for direct kubelet configuration. GKE's equivalent NodeKubeletConfig fields use integer `shutdownGracePeriodSeconds` (Spot-only, values 0/30/120) and `shutdownGracePeriodCriticalPodsSeconds`. This API deliberately supports on-demand direct-GCE nodes and retains #732's kubelet shape rather than copying GKE's managed node-pool restrictions. Configuring kubelet does not extend a VM's platform shutdown deadline or enable the extended Spot termination window.

Both durations must be non-negative, parse within Go's duration range, and satisfy critical-pods <= total. Critical-pods configuration requires an explicit total. Enforce syntax and field-level CEL parsing in generated CRDs plus the existing cross-field rule.

Provider policy remains in `pkg/providers/instance`; `pkg/metadata` remains a pure transformation layer. No new cloud API calls or permissions are required.

## Migration

Newly provisioned on-demand nodes no longer inherit source-pool shutdown policy. Operators depending on that policy must explicitly configure both durations on their GCENodeClasses; see `MIGRATION.md`. Existing nodes are not changed in place. There is no hash-version bump solely to force rollout; NodeClass changes retain normal drift behavior.

## Acceptance Criteria

- [ ] Rendered shutdown configuration is independent of source shutdown fields, while unrelated source configuration survives.
- [ ] On-demand uses 0s/0s and Spot uses 30s/15s when NodeClass fields are omitted.
- [ ] NodeClass overrides, partial totals, and explicit zero render valid kubelet pairs.
- [ ] Generated admission validation rejects standalone overflow and accepts valid Go duration boundaries.
- [ ] Documentation explains defaults, divergence from GKE's API, and migration.

## Test Plan

Render-level instance tests cover defaults and overrides against source configurations with ordinary and priority-based shutdown settings. CRD-tools tests evaluate generated schema validation and CEL with untyped values, including overflowing standalone total, maximum Go duration, malformed input, mixed units and invalid pairs.

A separately authorized staging test can configure an on-demand node, run a pod recording SIGTERM, trigger graceful OS shutdown, and verify termination and replacement using existing provisioning-suite cleanup. This crosses the kubelet/systemd/VM boundary and is not established by metadata tests; no live shutdown test is included or claimed here.

## Alternatives Considered

- **Source-pool inheritance**: preserves existing behavior but violates deterministic policy independent of bootstrap source selection.
- **30s/15s for every node**: consistently enables graceful shutdown, but changes the verified on-demand baseline and is not a universal documented GKE-on-GCP default.
- **GKE Spot-only integer API**: familiar for managed GKE pools, but excludes the on-demand use case and changes the original PR's upstream kubelet API shape.

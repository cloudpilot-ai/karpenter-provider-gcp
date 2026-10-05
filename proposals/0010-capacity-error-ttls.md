# Proposal: Shorter and Error-Specific TTLs for Unavailable Offerings

- **Status**: Draft
- **Authors**: @joemiller
- **Created**: 2026-09-28
- **Related Issues**: N/A

---

## Summary

When a launch fails with a capacity error other than IP space exhaustion, the provider marks the offering (`capacityType:instanceType:zone`) unavailable for 30 minutes, whether the error is a transient stockout or a configuration the zone does not support. This proposal sets a fixed TTL per error class:

| Error class                                                                          | Today | Proposed |
|--------------------------------------------------------------------------------------|-------|----------|
| Transient stockout                                                                   | 30m   | 5m       |
| Unsupported configuration (`configuration_availability`, `MACHINE_TYPE_UNSUPPORTED`) | 30m   | 1h       |
| IP space exhausted                                                                   | 30s   | 30s      |

No API, CRD, flag, or state changes.

---

## Motivation

Stockouts have become a routine part of provisioning on GCE. With a 30-minute TTL, an offering whose capacity returns after a few minutes stays excluded, and pods scheduled in that window go to other, possibly more expensive, offerings. Users who run Karpenter on more than one cloud expect similar behavior across providers:

| Implementation                                        | Capacity-error TTL / backoff                                                                                                            | Configurable                                |
|-------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------|---------------------------------------------|
| karpenter-provider-gcp                                | 30m; 30s for `IP_SPACE_EXHAUSTED*`                                                                                                      | No                                          |
| karpenter-provider-aws                                | 3m (`UnavailableOfferingsTTL`)                                                                                                          | No                                          |
| karpenter-provider-azure                              | 3m default; per-reason: 1h allocation failure, 1h spot `SKUNotAvailable`, 23h on-demand `SKUNotAvailable`, 10m low quota, 1h zero quota | No                                          |
| karpenter-provider-oci                                | 3m                                                                                                                                      | Yes (`--unavailable-offerings-ttl-seconds`) |
| Cluster Autoscaler (per node group, not per offering) | 5m initial, 30m max, reset after 3h                                                                                                     | Yes (flags)                                 |

GCP does not publish a retry interval for [resource availability errors](https://cloud.google.com/compute/docs/troubleshooting/troubleshooting-resource-availability); it says only to retry later, in another zone, or with another configuration. The values here are a best-effort alignment with the other Karpenter providers and with Cluster Autoscaler.

A shorter TTL should not apply to errors that do not resolve with time. GCE returns `ZONE_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS` both for stockouts and for unsupported configurations, and distinguishes them with `errorDetails[].errorInfo.reason` on the operation ([zoneOperations reference](https://cloud.google.com/compute/docs/reference/rest/v1/zoneOperations)). `configuration_availability` means the configuration is not supported in the zone. `MACHINE_TYPE_UNSUPPORTED` (#622) likewise does not resolve with time. The provider already extracts the reason into `insufficientCapacityDetails.structuredReason`, but selects the TTL from the top-level code only.

---

## Proposal

`insufficientCapacityBackoffTTL` takes the full details instead of the code:

```go
const (
	transientCapacityErrorTTL   = 5 * time.Minute
	unsupportedConfigurationTTL = 1 * time.Hour
)

func insufficientCapacityBackoffTTL(d insufficientCapacityDetails) time.Duration {
	switch {
	case d.code == "IP_SPACE_EXHAUSTED" || d.code == "IP_SPACE_EXHAUSTED_WITH_DETAILS":
		return ipSpaceInsufficientCapacityTTL
	case d.code == "MACHINE_TYPE_UNSUPPORTED", d.structuredReason == "configuration_availability":
		return unsupportedConfigurationTTL
	default:
		return transientCapacityErrorTTL
	}
}
```

- Unknown or missing reasons are treated as transient.
- Synchronous `Insert` errors carry no `errorInfo` in `googleapi.Error.Errors`, so they are classified by code only.
- Both classes still return an insufficient-capacity error, so core Karpenter reschedules without the offering. `newInsufficientCapacityError` writes `unsupported configuration` instead of `unavailable` in the provider's part of the message for that class; the `insufficient capacity` prefix comes from core and is unchanged.
- `unavailableofferings.DefaultTTL` (30m) is unchanged and remains in use for spot preemption.

The cache key has no NodeClass, so a 1h unsupported-configuration mark from one NodeClass excludes the offering for all NodeClasses, as the 30m mark does today.

Tests: a table test for `insufficientCapacityBackoffTTL`, message tests for both classes, and updates to the existing `30m0s` assertions in `instance_test.go`.

---

## Alternatives Considered

- **Exponential backoff** (5m up to 30m, as in Cluster Autoscaler): needs a per-offering failure counter that handles parallel launches and a reset-on-success rule. A flat TTL avoids that state.
- **3m, matching AWS**: each retry of a still-stocked-out offering adds a failed insert to a NodeClaim's launch before it falls through to the next instance type. GCP launches one instance type per call, while EC2 Fleet tries several in one call, so 5m makes those failed attempts less frequent.
- **Configurable TTL**: the OCI provider exposes one; the AWS and Azure providers do not. A single flag cannot express per-class values. A flag can be added later if needed.

---

## Open Questions

1. Should spot preemption move from 30m to 5m? The AWS provider uses its 3m TTL for preemption.

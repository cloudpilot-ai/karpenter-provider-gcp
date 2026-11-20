# Proposal: Release-aligned core node bootstrap

- **Status**: Draft
- **Authors**: @dm3ch
- **Created**: 2026-11-20
- **Related Issues**: [#789](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/789)

## Summary

Resolve a core Kubernetes bootstrap release with each node image, rather than inheriting that release from a managed source pool. New nodes use matching server binaries, manifests, kubelet version override and bootstrap scripts even while the source pool remains on an older release.

Source templates remain necessary for credentials, cluster context and runtime policy. This proposal deliberately retains `RENDERED_INSTALLABLES` unchanged under the accepted compatibility assumption; it does not reconstruct every GKE component or eliminate source-template discovery.

## Motivation

### Problem Statement

Image selection can advance independently of the managed source pool. The existing architecture patch reads the pool's server archive URL, preserves its release, and returns unchanged for matching architectures. Consequently, a newly selected image can receive an older kubelet and manifests.

Updating only the server URL would leave stale hashes, manifests, version overrides and entrypoints. Resolving a fresh master version at VM creation can also disagree with an image selected earlier and retained in status.

### Goals

- Carry one image/release selection through status, caching and launch.
- Rebuild known core release inputs without consulting source artifact values.
- Preserve image pins, source-pool preference, cluster context and runtime policy.
- Preserve adoption and existing image/spec-driven replacement behavior.

### Non-Goals

- Reconstructing installables or creating/upgrading version-specific bootstrap pools.
- Changing CSR identity, credentials, legacy VM identity recovery or automatic version-only drift.
- Migrating Ubuntu image catalogs or validating every Ubuntu1.36 image-discovery path.
- Upgrading running nodes or deploying the provider as part of this change.

## Proposal

### Image and release selection

Add optional `kubernetesVersion` to each `status.images[]` entry and its internal image result. Cache the entire selection, not an image combined later with an independently refreshed version.

For recognized COS images in the official `gke-node-images` publisher project, retain the release encoded in the selected image. Modern names concatenate a one-digit Kubernetes major, two-digit minor and variable-width patch, followed by the GKE build. Support multi-digit patches explicitly; do not decode custom-project lookalikes or ambiguous older formats. Channel, latest, milestone and explicit image pins retain their existing image-selection behavior.

For Ubuntu, capture one exact API-server version during resolution and use it both for the existing minor-scoped image lookup and the bootstrap release. Keep the selected OS date/image pin; the OS pin is not a kubelet-version pin. The separate Ubuntu1.36 image-project migration remains outside this proposal.

Opaque custom image IDs keep their image and architecture but use an exact API-server version snapshot. Operators are responsible for selecting an image compatible with that bootstrap release. This is explicit policy, not a guessed image-version decoder or a source-pool fallback.

A launch selects one compatible status row and passes its image and resolved bootstrap bundle together into disk and metadata construction. Shared image compatibility helpers remain version-agnostic because scheduling and drift also call them.

### Core artifact resolution

A small bootstrap provider accepts the already selected GKE release and architecture. It uses the canonical public release prefix:

```text
https://storage.googleapis.com/gke-release/kubernetes/release/v<release>/
```

Resolve these values:

| Surface                                              | Target-owned value                                                  |
|------------------------------------------------------|---------------------------------------------------------------------|
| `SERVER_BINARY_TAR_URL` / `SERVER_BINARY_TAR_HASH`   | Selected release's architecture-specific server archive and SHA-512 |
| `KUBE_MANIFESTS_TAR_URL` / `KUBE_MANIFESTS_TAR_HASH` | Same release's manifests archive and SHA-512                        |
| `KUBELET_VERSION`                                    | Same normalized release, with leading `v`                           |
| `configure-sh`                                       | Verified `extra/gce/configure.sh`                                   |
| `user-data`                                          | Verified `extra/gce/node.yaml`                                      |

The controller fetches only small scripts and hash sidecars. Existing node installers reuse matching preloads or download the exact archives. Verify script bytes against their sidecars, bound response sizes/time, coalesce identical requests and cache only complete successful bundles in a bounded cache. Sidecars establish consistency with the HTTPS distribution, not independent publisher signatures. A failed lookup is a dependency failure, not stockout; do not mark offerings unavailable or fall back to old source values.

### Metadata ownership

The pure metadata model stops importing the core fields above, legacy `SALT_TAR_URL`/`SALT_TAR_HASH`, and competing source startup entrypoints. The inspected node installer does not consume the SALT fields; remove them rather than inventing an artifact path.

The instance provider supplies a complete typed core bundle. The metadata package only applies/renders it; it performs no HTTP/GKE lookup or image-selection policy. Leave installables, inherited kubelet configuration, feature settings and bootstrap context unchanged.

Retaining installables is an explicit compatibility assumption, not a proof that arbitrary cross-version payloads are safe. If stricter synchronization becomes necessary and no independent producer is available, a separate design could select or bootstrap a zero-worker template pool at the target release. That pool lifecycle is not included here.

### Adoption and replacement

Keep artifact resolution and missing-status checks after existing-VM adoption. A previous VM remains adoptable during an artifact outage or before status refresh.

Do not include `kubernetesVersion` in image compatibility, NodeClass spec hashes, or drift decisions. A release-only refresh with the same image does not replace running nodes. Existing image/spec-driven replacement remains unchanged.

## Migration

1. Update provider CRDs before upgrading the controller. An old schema can otherwise prune `status.images[].kubernetesVersion` indefinitely.
2. Allow normal NodeClass reconciliation to refresh existing image rows. New VM creation waits for a complete row; adoption does not.
3. Allow controller HTTPS access to the canonical GKE release distribution for small scripts/sidecars. Nodes continue to need access to required archives unless matching preloads exist. This does not add a new mirror or alternate-universe configuration surface.

No automatic managed-pool upgrade or running-node migration is performed. Ubuntu and opaque custom images now use the captured master release instead of implicitly inheriting the source pool release.

## Test Plan

Hermetic regressions cover same-architecture and ARM release alignment, old/missing source artifact fields, exact selected-row propagation, retained installables/context, snapshot/cache/status coherence, custom-project lookalike names, hash/script failures and retries, old-status adoption and unchanged release-only drift. HTTP adapter tests use local/fake responses, never live GCP or credentials.

Run the applicable unit/race, lint, generated-code/CRD, chart and documentation checks. Real node registration crosses bootstrap boundaries and is useful integration coverage, but any deployment/live E2E requires separate explicit operation/environment approval; this proposal does not claim that validation.

## Acceptance Criteria

- A source1.35 template cannot choose core1.35 artifacts for a selected target1.36 launch.
- Boot disk and core metadata consume one selected image/release tuple.
- Both architectures use the selected release and their own server archive/hash.
- Installables and cluster/runtime context remain unchanged.
- Artifact failures cannot insert a VM with stale fallback artifacts or block earlier VM adoption.
- Existing pins, source preference and drift/disruption behavior are preserved as described.

## Alternatives Considered

- **Use master version only at VM build:** can mix a cached image with a newer version and breaks COS channel/pin semantics.
- **Only patch server binaries:** leaves old manifests, scripts or version overrides in place.
- **Require a matching managed source pool:** preserves the operational dependency that causes the defect and adds pool lifecycle/permissions.
- **Rebuild all GKE installables:** no verified public complete producer was found; outside the accepted narrow fix.
- **Reject mismatches as the principal fix:** prevents provisioning during ordinary source-pool lag rather than correcting bootstrap selection.

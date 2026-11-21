# Proposal: Ubuntu image catalog for GKE 1.36+, `osVersion` and Ubuntu 26.04

- **Status**: Draft
- **Authors**: @dm3ch
- **Created**: 2026-11-21
- **Related Issues**: [#789](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/789) (follow-up: [proposal 0013](0013-release-aligned-bootstrap.md) left the Ubuntu catalog migration out of scope)

## Summary

GKE publishes Ubuntu node images in `ubuntu-os-gke-cloud` for Kubernetes before 1.36 and in `gke-ubuntu-node-images` for 1.36 and later. Karpenter reads only the former. This proposal selects the image project from the cluster's Kubernetes minor, replaces the per-series regex copies with a release table, and keeps node drift stable across the project change. It also reshapes how an Ubuntu release is chosen: `family: Ubuntu` with a required `osVersion` (`"22.04"`, `"24.04"`, `"26.04"`) replaces one enum value per release, so Ubuntu 26.04 and later releases need no new family and no CRD change.

## Motivation

### Problem Statement

`pkg/providers/imagefamily/ubuntu.go` hardcodes `ubuntu-os-gke-cloud`. A read-only catalog listing on 2026-11-21 showed:

- The plain images `ubuntu-gke-2404-1-3{6,7}-<arch>-vYYYYMMDD[a]` currently exist in both projects with identical names and newest dates. Resolution on 1.36 and 1.37 works today, but Google documents the new project as the source, and the old one can stop receiving images.
- Ubuntu 26.04 images (`ubuntu-gke-2604-1-37-*`, `ubuntu-gke-2604-1-38-*`) exist in both projects. No `ubuntu-gke-2404-1-38-*` image exists, so Ubuntu 24.04 will not resolve on 1.38 clusters.
- Ubuntu 22.04 images exist only through Kubernetes 1.31 (the documented support range is 1.28+, so 22.04 remains usable on 1.28 to 1.31).
- New variant suffixes appear beside the existing ones (`-hwe`, `-64k`, `-acs`, besides `-tpu`, `-cgroupsv1`, `-linux64k`).
- `gke-ubuntu-node-images` also holds per-GKE-patch images named `gke-<rel>-gke<patch>-ubuntu-<release>-<minor>-<arch>-vDATE[-variant]-c-pre`. Their embedded release does not always agree with the `1-NN` token, so it is not a reliable release source.
- Node drift compares the node's boot image to `projects/<project>/global/images/<name>` by suffix. Moving the resolved project would drift every existing Ubuntu node, including pinned ones.

### Goals

- Resolve Ubuntu images from the project GKE uses for the cluster's minor.
- Select the Ubuntu release with a field instead of one family per release, and support 26.04 without a third copy of the 22.04/24.04 branching.
- Not replace existing Ubuntu nodes because of the project move.
- Fail clearly when GKE publishes no image of the selected release for the cluster's minor.

### Non-Goals

- Dropping Ubuntu 22.04 (still usable on the documented range up to 1.31) or removing the deprecated `Ubuntu2204`/`Ubuntu2404` family names, which keep working; removal follows the project API compatibility policy.
- Selecting Ubuntu images by exact GKE patch or release channel, or decoding releases from `-c-pre` names. Ubuntu keeps the API-server snapshot behavior from proposal 0013.
- Substituting an Ubuntu release or image project when the selected one has no image.
- Switching `Ubuntu@...` aliases away from Ubuntu 24.04.

## Proposal

### Project selection

For cluster Kubernetes `1.N`: `N >= 36` selects `gke-ubuntu-node-images`; otherwise `ubuntu-os-gke-cloud`. Only the selected project is listed. There is no cross-project fallback, so a misconfigured or blocked project fails visibly (`ImageResolutionFailed`) instead of silently resolving a different catalog. An unparseable cluster version already fails resolution and still does.

### Release table

A table keyed by release token (`2204`, `2404`, `2604`) holds the clean-name allowlists per architecture (22.04 amd64 has no arch token) and an optional hint appended to resolution failures (22.04: "GKE publishes Ubuntu 22.04 node images only through Kubernetes 1.31"). Allowlists stay anchored on the version end, so variants and `gke-*-c-pre` names are never `latest` candidates. A new release is one table row.

### `osVersion`

`imageSelectorTerms[].osVersion` (pattern `^[0-9]{2}\.[0-9]{2}$`) selects the Ubuntu release. Rules:

- `family: Ubuntu` requires `osVersion`; every other family rejects it. There is no default, so Karpenter never chooses an OS major, and changing it is an explicit image change that drifts nodes under the NodePool's disruption budgets. Proposal 0002 rejected a bare `Ubuntu` family for exactly this reason; the field provides the explicit release it asked for.
- `version` keeps meaning the build pin within the release (`latest` or `vYYYYMMDD[a-z]`). It cannot carry the release because builds of different releases share dates.
- `channel` stays COS-only.
- The name follows GKE `windowsNodeConfig.osVersion` ("OS version" selects the release of an OS family). GKE `imageType` has no Ubuntu release selector, so there is no closer equivalent.
- The format is an OpenAPI pattern, not an enum: a release Karpenter does not know yet is rejected at resolution with the supported releases listed, which avoids a CRD change (CRDs are upgraded separately from the controller) when a release is added.
- `Ubuntu2404` and `Ubuntu2204` are deprecated, frozen at those two values, and resolve exactly like `Ubuntu` with `osVersion: "24.04"`/`"22.04"`. The `Ubuntu@...` alias remains Ubuntu 24.04. No `Ubuntu2604` family is added: new releases exist only in the new shape. Switching a NodeClass between spellings does not drift nodes (`imageSelectorTerms` is excluded from the NodeClass hash and image drift compares image names).

All releases map to the internal `Ubuntu` family, so bootstrap and swap handling are shared. 26.04 bootstrap behavior is validated by E2E, not assumed.

### Drift across the Ubuntu projects

Image drift treats a node's boot image as the published image when the full path matches, or when both are in the two Ubuntu image projects and the image name is equal. A different name (build, arch, series) still drifts. Names are equal across the projects by GKE's publishing practice; the unit tests pin this contract.

## Risks and Mitigations

| Risk                                                                                     | Likelihood             | Impact                                       | Mitigation                                                                                                      |
|------------------------------------------------------------------------------------------|------------------------|----------------------------------------------|-----------------------------------------------------------------------------------------------------------------|
| Environments cannot read `gke-ubuntu-node-images` (trusted-image-project policy, VPC-SC) | Medium                 | Ubuntu `ImagesReady=False` on 1.36+          | `MIGRATION.md` and docs name the project; failure names it.                                                     |
| Google stops publishing a release for a minor (24.04 on 1.38)                            | High (certain on 1.38) | Existing NodeClass cannot resolve            | Clear error; docs list availability; no silent release switch.                                                  |
| A third spelling of the same selection adds API surface                                  | Medium                 | Confusing docs and more validation           | Old families frozen and deprecated; docs show only the new shape; removal follows the API compatibility policy. |
| `osVersion` accepts a release Karpenter does not know                                    | Medium                 | Failure surfaces in status, not at admission | Error lists supported releases; documented.                                                                     |
| Names diverge between projects                                                           | Low                    | Spurious drift or missed drift               | Name-equality contract tested; revisit if Google changes naming.                                                |
| Ubuntu 26.04 bootstrap differs                                                           | Medium                 | 26.04 nodes fail to register                 | E2E on a cluster with 26.04 images before claiming support.                                                     |

## Test Plan

### Unit Tests

- Project and prefix per minor (1.31, 1.35, 1.36, 1.37, 1.38) and unparseable version.
- Only the selected project is listed, even if the other holds a newer image.
- Each release x architecture accepts clean names (including letter suffix) and rejects variants, other releases, `-c-pre` and deprecated/non-READY images.
- 26.04 resolution beside `-hwe`/`-tpu`/`-64k`/`-acs`.
- A release with no image for the minor fails with project/release (and the 22.04 hint); an unknown `osVersion` lists the supported releases.
- `family`/`osVersion` routing: `Ubuntu`+`osVersion`, `Ubuntu2404`, `Ubuntu@latest` and `Ubuntu2404@latest` resolve identically for 24.04.
- CRD admission (generated CEL): `Ubuntu` requires `osVersion`, other families reject it, malformed releases are rejected, deprecated families still admit.
- Drift: same name from the other Ubuntu project does not drift; older build, other arch or unrelated project does.

### E2E / Integration Tests

- Existing Ubuntu provisioning/pinning suites on a 1.36+ cluster resolve from the new project (`ResolveCurrentUbuntuVersion` selects the project from the minor).
- `osVersion: "26.04"` provisioning where the E2E cluster has 26.04 images for its minor; requires separate consent for a live run.

## Migration

Allow the controller's project/network policy to use `gke-ubuntu-node-images` images on GKE 1.36+. Upgrade CRDs before the controller to get `osVersion`; existing `Ubuntu2404`/`Ubuntu2204` NodeClasses keep working and can be rewritten as `family: Ubuntu` with `osVersion` at any time without drift. Before upgrading a cluster to a minor with no image for the NodeClass's release, change `osVersion` to one that has images. Existing nodes are not replaced by the project change.

## Alternatives Considered

### Probe both projects and prefer the first hit

Rejected: hides blocked or misconfigured projects and makes the selected catalog depend on transient publication state.

### Select by exact GKE patch via `-c-pre` images

Rejected for now: the embedded release is not reliably decodable for Ubuntu and would change image pin and channel semantics.

## Future Direction

- Release-channel-aware Ubuntu selection once the `-c-pre` naming is understood.
- Dropping the legacy project once Google stops publishing there.

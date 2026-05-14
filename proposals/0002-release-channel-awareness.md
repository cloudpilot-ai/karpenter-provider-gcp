# Proposal: GKE Release Channel Awareness for Image Selection

- **Status**: Implemented
- **Authors**: @dm3ch
- **Created**: 2026-05-12
- **Related Issues**: [#330](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/330)

---

## TL;DR

```yaml
# Follow the cluster's enrolled channel — recommended starting point
imageSelectorTerms:
  - channel: cluster
    family: ContainerOptimizedOS

# Pin to a specific COS version — replaces alias: ContainerOptimizedOS@125.19216.104.126
imageSelectorTerms:
  - family: ContainerOptimizedOS
    version: "125.19216.104.126"

# Ubuntu 24.04 latest — replaces alias: Ubuntu@latest
imageSelectorTerms:
  - family: Ubuntu2404
    version: latest
```

Pair `channel:` terms with a disruption budget to control when node replacement happens after a GKE build promotion.

---

## Summary

After PR #280 introduced catalog-based image resolution, Karpenter selects the newest non-deprecated GKE OS image for the cluster's current Kubernetes version. This is correct for basic operation, but ignores GKE release channels (RAPID, REGULAR, STABLE, EXTENDED).

For COS images, different channels are validated against different GKE build numbers even on the same Kubernetes patch version. STABLE receives a build only after it has soaked in RAPID and REGULAR. With catalog-based selection Karpenter may assign a newly-published build to a STABLE cluster immediately — before it has been promoted to STABLE — defeating the operational purpose of being on that channel.

This proposal introduces three new structured fields in `ImageSelectorTerm` — `family`, `channel`, and `version` — that together replace the existing `alias` compound field:

- `family + channel` is a **live reference**: resolves to whichever build GKE currently promotes for the named channel. When GKE promotes a new build, existing nodes are marked Drifted and replaced according to configured disruption budgets.
- `family + version` is a **static pin**: resolves to a fixed, named image. Nodes never drift due to image changes.
- `family + version: latest` is a **latest reference**: picks the newest image for the cluster's current Kubernetes version, equivalent to the current `alias: *@latest` behaviour without channel awareness.

**`alias` is soft-deprecated as of this proposal.** All existing `alias:` and `id:` configurations continue to work. A future major release will remove the `alias` field once all use cases are covered by `family + channel` and `family + version`. See [Deprecation Path](#alias-deprecation-path).

> **Note:** `channel: stable` resolves to GKE's most-soaked build for the cluster's Kubernetes minor version. Because STABLE trails RAPID by weeks, `channel: stable` will typically resolve to an **older** build number than `version: latest` on the same cluster. This is intentional: STABLE's value is its validation history, not its age.

### Relationship to PR #263 and #280

PR #263 eliminated the dependency on NodePool instance templates as a bootstrap source. PR #280 introduced catalog-based image resolution, gaining independence from pool lifecycle. This proposal reconstructs channel-level image knowledge using a different public API (`getServerConfig`) rather than reverting to pool dependency — gaining channel awareness without sacrificing the progress made in #263 and #280.

---

## Motivation

### Problem Statement

GKE release channels promote OS images in order: RAPID → REGULAR → STABLE → EXTENDED. Each channel has a distinct GKE build number for a given Kubernetes patch version, visible in `getServerConfig`:

```
RAPID    defaultVersion = 1.35.3-gke.1737000   ← newest, least soaked
REGULAR  defaultVersion = 1.35.3-gke.1389000
STABLE   defaultVersion = 1.34.6-gke.1068000   ← most soaked, production-recommended
EXTENDED defaultVersion = 1.35.3-gke.1389000
```

Karpenter currently ignores these build numbers and picks the newest non-deprecated image for the cluster's Kubernetes version. A STABLE cluster may receive a build that was published hours ago and has only been tested in RAPID.

### What Public APIs Expose

**`getServerConfig` (GKE Container API)** returns per-channel `defaultVersion` and `validVersions`. The GKE build number in `defaultVersion` maps directly to COS image catalog entries — confirmed by cross-referencing live API output:

```
defaultVersion = 1.35.3-gke.1737000
→ catalog: gke-1353-gke1737000-cos-125-19216-220-130-c-pre   ← RAPID

defaultVersion = 1.34.6-gke.1068000
→ catalog: gke-1346-gke1068000-cos-125-19216-220-72-c-pre    ← STABLE
```

Conversion: `"{major}.{minor}.{patch}-gke.{build}"` → filter `gke-{major}{minor}{patch}-gke{build}-*`

**There is no channel-to-image mapping in any other public API.** Exhaustive audit confirmed: GCE Compute image labels, image families, image descriptions, and GCS release buckets carry no channel information.

### Ubuntu Does Not Support Release Channel Differentiation

Ubuntu GKE images (`ubuntu-gke-2404-{minor}-amd64-v{date}`) use date-based versioning and do not encode a GKE build number. There is no per-channel Ubuntu image distinction in any public GCP API — all channels on the same Kubernetes minor version share the same Ubuntu image pool.

**This is a hard limitation of the GKE public API, not a design choice.** The `channel` field is therefore only supported for `family: ContainerOptimizedOS` in this proposal. Ubuntu images are selected using `family: Ubuntu2404` or `family: Ubuntu2204` with `version: latest` (automatic) or a date pin (e.g. `version: "v20260416"`). Ubuntu channel support may be added in a future proposal if GKE surfaces per-channel Ubuntu image metadata.

---

## Proposal

### Static vs Live References: The Core Semantic Distinction

| Type                       | Example                                                          | Behaviour                                                                          |
|----------------------------|------------------------------------------------------------------|------------------------------------------------------------------------------------|
| **Version pin**            | `family: ContainerOptimizedOS`<br>`version: "125.19216.104.126"` | Fixed image. Nodes never drift from image changes.                                 |
| **Latest reference**       | `family: Ubuntu2404`<br>`version: latest`                        | Newest image for cluster's K8s version. No channel awareness.                      |
| **Live channel reference** | `family: ContainerOptimizedOS`<br>`channel: stable`              | Resolves to GKE's current STABLE build. Nodes drift when GKE promotes a new build. |

### API Change: New `family`, `channel`, and `version` Fields in `ImageSelectorTerm`

Three new optional fields are added alongside the unchanged `id` and the now-deprecated `alias`:

> **`alias` is deprecated.** New configurations should use `family + channel` or `family + version` instead. The `alias` field will be removed in a future major release. See the [Deprecation Path](#alias-deprecation-path) section for migration guidance.

```yaml
imageSelectorTerms:
  # Channel-following (live reference, COS only)
  - family: ContainerOptimizedOS
    channel: stable       # rapid | regular | stable | extended | cluster

  # COS version pin (static)
  - family: ContainerOptimizedOS
    version: "125.19216.104.126"

  # Ubuntu 24.04 latest
  - family: Ubuntu2404
    version: latest

  # Ubuntu 22.04 pinned to a specific date build
  - family: Ubuntu2204
    version: "v20260416"

  # Raw image ID — unchanged
  - id: projects/gke-node-images/global/images/gke-1351-gke1396004-cos-125-19216-104-126-c-pre
```

| `channel` value | Image version resolved                                                          |
|-----------------|---------------------------------------------------------------------------------|
| `rapid`         | `RAPID.defaultVersion` build number                                             |
| `regular`       | `REGULAR.defaultVersion` build number                                           |
| `stable`        | `STABLE.defaultVersion` build number                                            |
| `extended`      | `EXTENDED.defaultVersion` build number                                          |
| `cluster`       | Reads `cluster.ReleaseChannel.Channel` then resolves as the named channel above |

**`family` enum:**

| `family` value         | OS           | Notes                                                                   |
|------------------------|--------------|-------------------------------------------------------------------------|
| `ContainerOptimizedOS` | COS          | Supports `channel` and `version`                                        |
| `Ubuntu2404`           | Ubuntu 24.04 | Supports `version` only; `channel` not supported                        |
| `Ubuntu2204`           | Ubuntu 22.04 | Supports `version` only; `channel` not supported                        |
| `Ubuntu`               | —            | **Deprecated.** Rejected at admission; use `Ubuntu2404` or `Ubuntu2204` |

**`version` field values by family:**

| `family`               | `version: latest`                           | Version pin format            | Example pin         |
|------------------------|---------------------------------------------|-------------------------------|---------------------|
| `ContainerOptimizedOS` | Newest COS for cluster's K8s version        | `milestone.build.build.build` | `125.19216.104.126` |
| `Ubuntu2404`           | Newest Ubuntu 24.04 for cluster's K8s minor | `vYYYYMMDD`                   | `v20260416`         |
| `Ubuntu2204`           | Newest Ubuntu 22.04 for cluster's K8s minor | `vYYYYMMDD`                   | `v20231201`         |

**Per-term CEL validation rules** (evaluated in order; first failure wins):

```
// 1. family requires channel or version (not standalone)
rule: "!has(self.family) || has(self.channel) || has(self.version)"
message: "family requires either channel or version to be set"

// 2. channel and version each require family
rule: "!(has(self.channel) || has(self.version)) || has(self.family)"
message: "channel and version require family to be set"

// 3. channel and version are mutually exclusive
rule: "!(has(self.channel) && has(self.version))"
message: "channel and version cannot both be set"

// 4. channel is only valid for ContainerOptimizedOS
rule: "!has(self.channel) || self.family == 'ContainerOptimizedOS'"
message: "channel is only supported for family ContainerOptimizedOS"

// 5. exactly one selection mode per term: alias, id, or family-based
rule: "(has(self.alias) ? 1 : 0) + (has(self.id) ? 1 : 0) + (has(self.family) ? 1 : 0) == 1"
message: "exactly one of alias, id, or family (with channel or version) must be set"

// 6. ContainerOptimizedOS version format
rule: "!has(self.version) || self.family != 'ContainerOptimizedOS' || self.version == 'latest' || self.version.matches('^[0-9]+\\.[0-9]+\\.[0-9]+\\.[0-9]+$')"
message: "ContainerOptimizedOS version must be 'latest' or 'milestone.build.build.build' (e.g. '125.19216.104.126')"

// 7. Ubuntu version format
rule: "!has(self.version) || !(self.family in ['Ubuntu2204', 'Ubuntu2404']) || self.version == 'latest' || self.version.matches('^v[0-9]{8}$')"
message: "Ubuntu version must be 'latest' or 'vYYYYMMDD' (e.g. 'v20260416')"

// 8. family: Ubuntu is not valid — require explicit release
rule: "!has(self.family) || self.family != 'Ubuntu'"
message: "family 'Ubuntu' is not valid; use Ubuntu2404 for Ubuntu 24.04 or Ubuntu2204 for Ubuntu 22.04"
```

Note: `Family`, `Channel`, and `Version` must be declared `omitempty` in Go structs so that `has()` in CEL correctly distinguishes an absent field from an empty string.

**List-level validation** on `spec.imageSelectorTerms` (applied via `+kubebuilder:validation:XValidations` on the field, where `self` is the list):

```
// 10. channel: and version: latest are mutually exclusive across the entire list for ContainerOptimizedOS
rule: "!(self.exists(t, has(t.channel)) && self.exists(t, has(t.family) && t.family == 'ContainerOptimizedOS' && has(t.version) && t.version == 'latest'))"
message: "channel: and version: latest cannot both appear for ContainerOptimizedOS in the same imageSelectorTerms; channel: is channel-validated, version: latest is channel-agnostic — use only one selection mode per OS family"
```

This rule is only needed for `ContainerOptimizedOS` because `channel:` is already restricted to that family by per-term rule 5. No equivalent mixing is possible for Ubuntu families.

### Version Resolution Algorithm

The following algorithm resolves a channel name to a GKE build number for catalog filtering. Step 1 is the normal operational path — it applies whenever the cluster and the requested channel are on the same Kubernetes minor version (the common production case). Steps 2–3 cover version-skew scenarios (upgrade windows and cross-channel references).

```
resolve(channel, cluster_minor):
  preamble (channel: cluster only):
    if cluster.ReleaseChannel is nil or UNSPECIFIED:
      ImagesReady = False
      error: "cluster has no enrolled release channel (UNSPECIFIED);
              channel: cluster requires an enrolled channel —
              enrol the cluster in a release channel or use version: latest explicitly"
      return
    channelName = strings.ToUpper(cluster.ReleaseChannel.Channel)

  1. channel.defaultVersion minor == cluster_minor
     → use defaultVersion build number
     → COS catalog filter: gke-{k8sKey}-gke{build}-*
       where k8sKey = major+minor+patch digits (e.g. 1.34.6 → "1346")

  2. minor mismatch (upgrade window or cross-channel reference)
     → filter channel.validVersions by cluster_minor using semver comparison
     → pick highest semver match
     → apply step 1 logic to that version

  3. no validVersions entry matches cluster_minor
     → ImagesReady = False
     → error: "channel STABLE has no valid version for cluster minor 1.35;
               the requested channel may not yet support this Kubernetes minor —
               switch to a channel that does, or use version: latest"
```

The preamble runs before step 1 because an UNSPECIFIED cluster has no channel name to look up in `getServerConfig` — it cannot naturally fall through the algorithm. Step 3 covers only the case where a named channel exists in `getServerConfig` but has no `validVersions` entry matching the cluster's Kubernetes minor. Both are operator configuration errors; silent fallback would mask the misconfiguration. `version: latest` is always available as an explicit opt-in for channel-agnostic selection.

`validVersions` must be compared using semver, not lexicographic order (`1.34.10 > 1.34.9` in semver; the reverse holds lexicographically).

**Channel name normalization:** `cluster.ReleaseChannel.Channel` from the GKE API uses uppercase (`"STABLE"`, `"RAPID"`). The `channel:` YAML field uses lowercase. Internally, all channel name comparisons use uppercase after normalizing the YAML value with `strings.ToUpper`. The `ServerConfig.Channels[].Channel` field from `getServerConfig` is also uppercase, so the comparison is consistent throughout.

If `defaultVersion` is empty or unparseable, the controller falls through to step 2 and attempts resolution from `validVersions`. This treats a missing `defaultVersion` as a minor-mismatch case rather than a hard failure — if `validVersions` contains a matching entry the resolution still succeeds. Only if step 2 also finds no match does step 3 fire.

If `getServerConfig` returns a non-error response but with an empty or missing `channels` list, or if the requested channel name is not present in the list, the controller sets `ImagesReady = False` with message `"channel <name> not found in getServerConfig response"` and retries.

### Drift and Disruption Budget Integration

When GKE promotes a new build for a channel, `getServerConfig` returns a new `defaultVersion`. After the 30-minute cache expires, the `GCENodeClass` controller re-resolves images and, if the resolved image changed, updates `Status.Images`. Karpenter's drift detector then marks nodes whose running image no longer matches. Replacement follows configured disruption budgets.

> **Restart behaviour:** If the Karpenter controller is restarted shortly after GKE promotes a new build (e.g., during a Karpenter upgrade), it starts with an empty cache, immediately fetches `getServerConfig`, and may mark all `channel:`-selected nodes as Drifted in a single reconcile pass. Disruption budgets cap the blast radius — this is the primary reason to pair `channel:` terms with a budget.

The same drift sequence occurs when the cluster's enrolled channel changes at the GKE level (e.g., the cluster is migrated from RAPID to STABLE). With `channel: cluster`, the next `GetClusterConfig()` cache expiry causes Karpenter to resolve images for the new channel — which may differ from the current nodes' images. This is expected behaviour, but operators should be aware that a GKE-side channel change will trigger node replacement on Karpenter's next reconcile cycle without any change to the `GCENodeClass`.

**Typical operational week with `channel: cluster` on a STABLE cluster:**

```
Monday:    operator sets channel: cluster; nodes use build gke1068000
Tuesday:   GKE promotes build gke1089000 to STABLE
Tue +30m:  cache expires; Karpenter resolves gke1089000; Status.Images updated
Tue +31m:  drift detector marks nodes as Drifted
Thursday:  nodes replaced during maintenance window per disruption budget
Next week: no GKE promotion → no image change → no drift → quiet
```

**Restrict replacement to a maintenance window:**

```yaml
disruption:
  budgets:
    - nodes: "10"
      reasons: [Drifted]
      schedule: "0 15 * * 2-4"   # Tue–Thu 15:00 UTC
      duration: 20m
```

**Receive new channel images but never trigger explicit drift eviction** (nodes rotate gradually through consolidation and expiration):

```yaml
disruption:
  budgets:
    - nodes: "0"
      reasons: [Drifted]
```

### `getServerConfig` Caching

30-minute TTL per controller restart. Channel version assignments change at most once per weekly GKE release cycle; a 30-minute lag carries no correctness risk (old images remain valid; running nodes are unaffected).

`GetClusterConfig()` (used by `channel: cluster`) is cached independently with its own 30-minute TTL. The two caches may expire at different times; a brief inconsistency (stale channel name + fresh server config) is harmless — it resolves on the next reconcile cycle.

If `GetServerConfig()` fails (network error, VPC Service Controls restriction), the controller sets `ImagesReady = False` and retries. If `GetClusterConfig()` fails specifically for `channel: cluster`, the controller also sets `ImagesReady = False` — without a cluster config the channel name cannot be resolved, so the failure is surfaced rather than silently degraded.

### COS Exact-Match Filter

When the resolved version carries a GKE build number, the catalog query uses an exact-build filter:

```
name=gke-1346-gke1068000-*    ← channel-exact build (step 1)
```

If the catalog returns no images (publication delay or regional propagation gap), the controller sets `ImagesReady = False` and retries on the next reconcile cycle. There is no silent fallback to the version-prefix filter (`name=gke-1346-*`): that fallback would pick whatever build is newest in the catalog — potentially a RAPID build that has never soaked in STABLE — violating the channel isolation guarantee that is the core purpose of this feature.

**Trade-off**: A brief `ImagesReady = False` window (typically seconds to a few minutes while the build propagates to the regional catalog) blocks new node provisioning until the exact build appears. This is preferable to silently provisioning nodes on the wrong channel's image.

### GPU and arm64 Variants

The same build-number filter applies to GPU and arm64 images. The existing suffix-expansion logic in `containeroptimizedos.go` (which appends `-arm64` and `-gpu` qualifiers) is unchanged; the build-number prefix selects the correct channel-validated variant automatically.

---

## `alias` Deprecation Path

The `alias` field is a compound string that encodes two semantically different operations — version pinning (`@125.19216.104.126`) and live selection (`@latest`) — in a single field. This conflation makes it harder to validate, document, and extend. The structured `family + channel / version` API introduced by this proposal covers every `alias` use case with explicit, independently-validated fields.

**`alias` is soft-deprecated as of this proposal.** It remains functional and will continue to work. All existing `GCENodeClass` resources using `alias:` or `id:` terms pass all new per-term and list-level CEL rules without any changes — the new rules were designed to be strictly additive. The field will be marked deprecated via a `// Deprecated:` Go doc comment on the struct field (the standard kubebuilder mechanism; there is no first-class CRD field-deprecation marker) and a controller log warning on use, in a future minor release. Removal is deferred to a future major release.

The migration from `alias` to structured fields requires no behavioral change for pinned versions; for `@latest`, the structured equivalent is `version: latest` (identical semantics) or `channel: cluster` (upgraded semantics — gains channel awareness).

| Deprecated config                               | Structured equivalent                                            | Notes                                                                    |
|-------------------------------------------------|------------------------------------------------------------------|--------------------------------------------------------------------------|
| `alias: ContainerOptimizedOS@latest`            | `family: ContainerOptimizedOS`<br>`version: latest`              | Exact semantic equivalent                                                |
| `alias: ContainerOptimizedOS@latest`            | `family: ContainerOptimizedOS`<br>`channel: cluster`             | **Recommended upgrade** — gains channel-aware image selection            |
| `alias: ContainerOptimizedOS@125.19216.104.126` | `family: ContainerOptimizedOS`<br>`version: "125.19216.104.126"` | Exact equivalent; resolves to same image                                 |
| `alias: Ubuntu@latest`                          | `family: Ubuntu2404`<br>`version: latest`                        | Exact equivalent; `Ubuntu2404` is the current GKE default Ubuntu release |
| `alias: Ubuntu@v20260416`                       | `family: Ubuntu2404`<br>`version: "v20260416"`                   | Exact equivalent; use `Ubuntu2204` if the pin targets a 22.04 build      |
| `family: Ubuntu` (bare, without release)        | `family: Ubuntu2404` or `family: Ubuntu2204`                     | Rejected at admission — specify the Ubuntu release explicitly            |

**Recommended migration path:** Replace `alias: ContainerOptimizedOS@latest` with `family: ContainerOptimizedOS, channel: cluster` and add a maintenance-window disruption budget. For pinned COS versions and Ubuntu, the migration is a mechanical substitution with no behavioral change. Ubuntu users should verify which release (22.04 or 24.04) their existing images target before substituting the `family` value.

---

## Observability

`GCENodeClass.Status.Images` is populated with resolved image URLs as normal. `Status.Conditions.ImagesReady` message records the resolution path taken — these messages are normative (implementations must include the stated key phrases):

| Situation                                       | Status.Conditions.ImagesReady message                                                                                                               |
|-------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------|
| Normal — exact build found                      | `ContainerOptimizedOS: resolved channel STABLE build gke1068000 → gke-1346-gke1068000-cos-...`                                                      |
| Exact build not in catalog (propagation delay)  | `ContainerOptimizedOS: build gke1068000 not yet in catalog; retrying until propagation completes` — `ImagesReady = False`                           |
| Step 3 — explicit channel, no minor match       | `ContainerOptimizedOS: channel STABLE has no valid version for cluster minor 1.35; the requested channel may not yet support this Kubernetes minor` |
| `channel: cluster` on UNSPECIFIED cluster       | `ContainerOptimizedOS: cluster has no enrolled release channel (UNSPECIFIED); channel: cluster requires an enrolled channel`                        |
| `GetServerConfig` failure                       | `getServerConfig failed: <error>; image resolution suspended`                                                                                       |
| `GetClusterConfig` failure (`channel: cluster`) | `GetClusterConfig failed: <error>; image resolution suspended`                                                                                      |
| Channel name not in server config               | `channel STABLE not found in getServerConfig response; image resolution suspended`                                                                  |
| Ubuntu version pin not in catalog               | `Ubuntu2404: image version v20260416 not found in catalog; check that the version exists for cluster minor 1.35`                                    |

---

## Implementation Plan

| File                                                | Change                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
|-----------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `pkg/providers/gke/gke.go`                          | Add `GetServerConfig()` + 30-min cache; add `ResolveVersionForChannel(sc, channelName, clusterVersion string) (string, error)` with semver-correct `validVersions` chain, empty-`defaultVersion` guard, and channel-not-found error; normalize channel names to uppercase; handle nil `cluster.ReleaseChannel` as UNSPECIFIED                                                                                                                                                                                                                                                                                                                                                           |
| `pkg/apis/v1alpha1/gcenodeclass.go`                 | **Update existing list-level rule first** — the current `imageSelectorTerms` field carries `rule="self.all(x, has(x.alias)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| `pkg/providers/imagefamily/image.go`                | Add `resolveImageFromChannel()` and `resolveImageFromVersion()` dispatchers. Dispatch order: iterate over all `imageSelectorTerms`; for each term dispatch based on which selection field is set (`alias`, `id`, or `family`); collect results per-term and union them into `Status.Images` — terms are ORed, matching existing Karpenter selector semantics. A single list may mix `alias`, `id`, and `family` terms; there is no priority between term types. The `Alias()` helper returns nil when no `alias` term is present — the dispatch loop must not assume a non-nil alias. Call `ResolveVersionForChannel` for channel terms; pass resolved version to COS/Ubuntu providers. |
| `pkg/providers/imagefamily/containeroptimizedos.go` | Add exact-build-number filter path for channel resolution; add version-pin lookup path for `family + version`; return `ImagesReady = False` (no version-prefix fallback) when exact build is not in catalog; include resolution path in `Status.Conditions` message                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `pkg/providers/imagefamily/ubuntu.go`               | Dispatch on `family: Ubuntu2404` and `family: Ubuntu2204`; add version-pin lookup path; resolve correct image project prefix per release (`ubuntu-gke-2404-*` vs `ubuntu-gke-2204-*`)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| `pkg/operator/operator.go`                          | Wire `gkeProvider` into image provider (if not already present)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| CRD schema                                          | Regenerate after struct changes                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |

No changes needed to `nodepooltemplate.go`. After PR #263 lands, `resolveNodePoolName()` is removed from `instance.go` entirely (that function was the other code path that required `ImageFamily()` to return a known value); no further `instance.go` changes are needed for this proposal.

---

## Non-Goals (explicit)

- **Ubuntu channel differentiation**: not possible with current GKE public APIs; `channel` field is COS-only in this proposal. Ubuntu 22.04 and 24.04 are distinct `family` values (`Ubuntu2204`, `Ubuntu2404`) but share the same image pool per K8s minor within each release.
- **Per-NodePool channel differentiation**: a single `GCENodeClass` applies one channel resolution to all NodePools that reference it. Operators needing different channels for different workloads use separate `GCENodeClass` objects.
- **Automatic channel migration pre-fetch**: `channel: cluster` reflects the cluster's enrolled channel at resolution time; it does not pre-fetch images before a pending channel change.
- **GKE Autopilot clusters**: karpenter-provider-gcp does not support Autopilot clusters (Autopilot manages its own node pool lifecycle). This non-goal is listed for completeness.
- **`alias` removal**: this proposal soft-deprecates `alias` but does not remove it. Removal is deferred to a future major release to allow operators time to migrate.

---

## Alternatives Considered

### Extend `alias` string with `@stable`, `@rapid` etc.

`alias: ContainerOptimizedOS@stable` — minimal API surface but conflates two semantically different operations: version pinning (`@125.19216.104.126`, static) and channel following (`@stable`, live). The structured `family + channel / version` approach makes the static/live distinction visible in the schema, enables independent CEL validation per field type, and provides a clean path to deprecating the `alias` string entirely.

### Allow `channel` for Ubuntu with documented limitation

Allowing `channel: stable, family: Ubuntu2404` with a warning in `Status.Conditions` when channels share the same K8s minor was considered. Rejected because it creates an API that appears to do more than it does. Ubuntu users use `family: Ubuntu2404, version: latest` until the GKE API exposes per-channel Ubuntu differentiation.

### Single `Ubuntu` family value with separate `release` field

```yaml
family: Ubuntu
release: "24.04"
```

A dedicated `release` field alongside `family` keeps `family` broad and avoids baking the release version into the enum. Rejected because it introduces a field only meaningful for Ubuntu (COS has no equivalent `release` concept), requiring Ubuntu-specific CEL rules. Encoding the release in the `family` value keeps the schema flat — `family` fully identifies the OS, `channel`/`version` select within it. When Ubuntu 26.04 ships it is a new enum value, which is a smaller change than a new field.

### Read image from existing NodePool instance template

Before PR #280, Karpenter read the boot disk image from the source pool's MIG instance template — genuinely channel-aware (the pool reflects exactly what GKE deployed for that channel). This requires an existing pool (the dependency PR #263 eliminated) and couples image resolution to pool lifecycle. Rejected.

### Top-level `spec.imageChannel` field on `GCENodeClass`

Simpler schema but applies one channel to all `imageSelectorTerms`, preventing per-term mixed selection. Rejected in favor of per-term selection.

### Use `upgradeTargetVersion` instead of `defaultVersion`

`ReleaseChannelConfig` in the GKE SDK exposes `upgradeTargetVersion` alongside `defaultVersion`. `upgradeTargetVersion` represents the version the channel is targeting for its next upgrade — it may be ahead of `defaultVersion` and could be used to pre-warm images before a cluster upgrade. Rejected for this proposal: `defaultVersion` reflects what GKE currently deploys for new nodes, which is the correct semantics for image selection. `upgradeTargetVersion` is a future direction if proactive pre-warming becomes a requirement.

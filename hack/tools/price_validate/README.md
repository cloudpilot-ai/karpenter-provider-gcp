# price_validate

This tool checks the quality of machineType price generation in
`karpenter-provider-gcp`. It compares the hourly prices computed by
[`instanceprice`](../../pkg/providers/pricing/instanceprice/) against two independent
reference sources simultaneously, making it easy to catch regressions in pricing
accuracy before they ship.

In case of a **Missing**, **Mismatch**, or **Extra** result, please research the
issue. Note that both reference sources can themselves contain mistakes:

- **cloud.google.com** pricing pages — scraped from embedded HTML/JSON; may lag
  behind actual billing changes or reflect a different pricing tier in some regions.
- **Cyclenerd price calculator** (`gcloud-compute.com`) — community-maintained
  reference CSV; generally accurate but not an official Google source.

In case of a mismatch between these two reference sources, it is better to check
the actual node price in the **Google Cloud Console UI** as the golden source —
it reflects actual billing and cannot be automatically scraped.

---

## How it works

Every run automatically executes three phases:

**Phase 1 — Reference prices**

Fetches both reference sources in parallel and saves them to
`data/cyclenerd.json` and `data/gcpweb.json`. Cached results are reused until
they exceed `--cache-ttl` (default 12 h).

**Phase 2 — Computed prices**

Calls `instanceprice.FetchAllPrices()` and gets Karpenter calculated prices for each machine in every selected region.

**Phase 3 — Compare**

Diffs computed prices against both reference sources (GCP web is authoritative, Cyclenerd is secondary) and prints any discrepancies.

---

## Usage

```sh
go run ./hack/tools/price_validate
```

The GCP project is resolved in this order:
1. `--project` flag
2. `$GOOGLE_CLOUD_PROJECT` / `$GCLOUD_PROJECT` environment variable
3. Application Default Credentials (`project_id` in service account JSON)

If none of the above work, run:
```sh
gcloud auth application-default login
export GOOGLE_CLOUD_PROJECT=<your-project-id>
```

## Flags

| Flag          | Default  | Description                                                        |
|---------------|----------|--------------------------------------------------------------------|
| `--project`   | (auto)   | GCP project ID (auto-detected from env or ADC if unset)            |
| `--region`    | `all`    | Region to compare, or `all`                                        |
| `--tolerance` | `0.01`   | Max fractional price diff (1%) before flagging                     |
| `--no-cache`  | `false`  | Ignore all caches and fetch everything fresh                       |
| `--work-dir`  | `./data` | Directory for cache and output files                               |
| `--cache-ttl` | `12h`     | Max age of reference price caches before re-fetching               |

---

## Output format

```
MISMATCH  n2-standard-8            us-central1  OnDemand computed=0.388000    gcp_web=0.388000(ok)    cyclenerd=0.400000(+3.1%)
MISMATCH  n2-standard-8            us-central1  Spot     computed=0.050000    gcp_web=0.055000(-9.1%) cyclenerd=n/a
MISSING   c4-standard-2            europe-west1 OnDemand computed=n/a         gcp_web=0.250000        cyclenerd=0.248000
EXTRA_NEW x5-experimental-4        us-east1     OnDemand computed=0.310000    gcp_web=n/a             cyclenerd=n/a

Summary over 37 region(s): checked=1234  mismatches=1  missing=1  extra=30  extra_new=1  unavail=335  blacklisted=0  tolerance=1%
```

- `MISMATCH`   — our price deviates from the authoritative reference (GCP web) by more than the tolerance. `(ok)` = agrees within tolerance; `n/a` = source has no data.
- `MISSING`    — we have no computed price for a machine that a reference source lists. This indicates a genuine pricing gap.
- `EXTRA`      — we computed a price for a machine neither reference source lists, but the machine type is in the `knownExtras` set in `main.go` (manually validated). Silently counted in the summary only.
- `EXTRA_NEW`  — same as EXTRA but the machine type is **not** in `knownExtras`. This needs investigation: validate the price in the GCP Console, then add the machine to `knownExtras` and to the Known EXTRA entries table below.
- `UNAVAIL`    — a reference source lists a price but the machine type is not deployed in the region according to the Compute Engine `machineTypes` API. Silently counted in the summary only.
- `BLACKLIST`  — the machine type is intentionally excluded from pricing. Silently counted in the summary only.
- **Exit code is always `0` (warn-only mode)** while the pricing implementation is being stabilised. Findings are printed for visibility but do not fail the tool.
---

## Known EXTRA entries

EXTRA means we compute a price but neither Cyclenerd nor GCP web has one. These
are legitimate machine types whose prices cannot be cross-validated automatically.
Each entry in `knownExtras` (in `main.go`) must have a row in this table.

| Machine type | Notes |
|---|---|
| `a3-edgegpu-8g` | A3 Edge — 8× NVIDIA H100 for serving. Not on standard pricing page. Ref: https://cloud.google.com/compute/docs/gpus |
| `a3-edgegpu-8g-nolssd` | Variant without local SSD. Same VM-level price as base — see note below. |
| `a3-megagpu-8g` | Cyclenerd includes it but prices differ in a few regions; absent from some regions entirely. Treat as unverified until a full re-run confirms. |
| `a3-ultragpu-8g` | Base A3 Ultra variant. Absent from reference sources in some regions (e.g. europe-west4, us-south1). |
| `a3-ultragpu-8g-nolssd` | Variant without local SSD. Base price differs by ~$1.3/hr (local SSD component) — see note below. |

### `-nolssd` variant pricing

The Compute API returns both `a3-*-8g` and `a3-*-8g-nolssd` variants. The
`-nolssd` variant has no local SSD in its machine spec, so the local SSD billing
component is $0. This means:

- `a3-edgegpu-8g` = `a3-edgegpu-8g-nolssd` ($87.83) — base has no SSD in the
  Compute API `scratchDisks` field either, so both are identical.
- `a3-ultragpu-8g` ($84.81) > `a3-ultragpu-8g-nolssd` ($83.49) — base includes
  local SSD at ~$1.3/hr (varies by region).

### Manual validation results (GCP Console, 2026-03-25)

| Machine type | Region | Console $/hr | Computed $/hr | Diff | Status |
|---|---|---|---|---|---|
| `a3-edgegpu-8g` | us-central1 | $88.49 | $87.83 | −0.74% | ✅ (SSD component underpriced by ~$0.66/hr) |
| `a3-edgegpu-8g-nolssd` | us-central1 | $87.83 | $87.83 | 0.00% | ✅ |
| `a3-ultragpu-8g` | us-central1 | $84.81 | $84.81 | 0.00% | ✅ |
| `a3-ultragpu-8g-nolssd` | us-central1 | $83.49 | $83.49 | 0.00% | ✅ |
| `a3-ultragpu-8g` | europe-west4 | $10.91 | $10.90 | −0.09% | ✅ ($0 GPU — reservation-bound region) |
| `a3-ultragpu-8g-nolssd` | europe-west4 | $9.46 | $9.46 | 0.00% | ✅ ($0 GPU — see note above) |

### Known MISMATCH — GCP web pricing page errors

The following machine types consistently show MISMATCH against `gcp_web` but agree with Cyclenerd. **Our prices are correct.** The GCP web pricing page has incorrect local SSD data for these specific machines (uses a much lower per-GiB rate than all other c4d-lssd variants).

| Machine type | GCP web $/hr | Computed $/hr | Notes |
|---|---|---|---|
| `c4d-highmem-8-lssd` | $0.745196 | $1.272950 | GCP web local SSD underpriced; Cyclenerd agrees with computed. |
| `c4d-highmem-16-lssd` | $1.384842 | $2.123697 | Same root cause. |

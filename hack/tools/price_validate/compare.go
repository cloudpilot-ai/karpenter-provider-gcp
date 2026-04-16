/*
Copyright 2026 The CloudPilot AI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"cmp"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
)

type ResultKind string

const (
	ResultOK        ResultKind = "OK"
	ResultMismatch  ResultKind = "MISMATCH"
	ResultMissing   ResultKind = "MISSING"
	ResultExtra     ResultKind = "EXTRA"
	ResultExtraNew  ResultKind = "EXTRA_NEW"
	ResultUnavail   ResultKind = "UNAVAIL"
	ResultBlacklist ResultKind = "BLACKLIST"
)

// priceKind identifies which price tier a result refers to.
type priceKind string

const (
	priceKindOnDemand priceKind = "OnDemand"
	priceKindSpot     priceKind = "Spot"
)

type result struct {
	kind        ResultKind
	priceType   priceKind
	region      string
	machineType string
	computed    float64
	refs        []float64 // one entry per reference source, 0 = source has no price
	// deviation is the signed fractional difference of computed relative to the
	// authoritative (first non-zero) ref: positive when computed > ref. Only
	// meaningful for ResultOK and ResultMismatch; zero for all other kinds.
	deviation float64
}

// buildResults diffs computed prices against ordered reference sources and
// returns the complete set of comparison results alongside the number of price
// comparisons performed. refs is ordered by trust: refs[0] is the most authoritative.
// availability maps region → machine type → true for every machine returned by
// the Compute Engine API; nil disables the ResultUnavail classification.
// isKnownExtra returns true for machine types that are intentionally absent from
// all reference sources (e.g. newly launched hardware not yet listed).
func buildResults(
	computedPrices RegionPrices,
	refs []RegionPrices,
	availability RegionAvailability,
	regions []string,
	isKnownExtra func(string) bool,
	tolerance float64,
) []result {
	var results []result

	for _, region := range regions {
		computedEntry := computedPrices[region]

		// Pre-compute per-ref region entries to avoid repeated map lookups.
		refEntries := make([]RegionEntry, len(refs))
		for i, ref := range refs {
			refEntries[i] = ref[region]
		}

		// Build union of all machine names present in any reference source
		// (on-demand is authoritative for machine existence).
		allRefNames := map[string]bool{}
		for _, entry := range refEntries {
			for name := range entry.OnDemand {
				allRefNames[name] = true
			}
		}

		for _, machine := range slices.Sorted(maps.Keys(allRefNames)) {
			refPrices := make([]float64, len(refEntries))
			for i, entry := range refEntries {
				refPrices[i] = entry.OnDemand[machine]
			}

			computedOD, ok := computedEntry.OnDemand[machine]
			if !ok {
				results = append(results, result{
					kind:        classifyMissing(machine, region, availability),
					priceType:   priceKindOnDemand,
					machineType: machine,
					region:      region,
					refs:        refPrices,
				})
				continue
			}

			odDev := authoritativeDeviation(computedOD, refPrices)
			odKind := ResultOK
			if math.Abs(odDev) > tolerance {
				odKind = ResultMismatch
			}
			results = append(results, result{
				kind:        odKind,
				priceType:   priceKindOnDemand,
				machineType: machine,
				region:      region,
				computed:    computedOD,
				refs:        refPrices,
				deviation:   odDev,
			})

			spotRefs := make([]float64, len(refEntries))
			for i, entry := range refEntries {
				spotRefs[i] = entry.Spot[machine]
			}
			if slices.ContainsFunc(spotRefs, func(p float64) bool { return p > 0 }) {
				computedSpot := computedEntry.Spot[machine]
				if computedSpot == 0 {
					results = append(results, result{
						kind:        classifyMissing(machine, region, availability),
						priceType:   priceKindSpot,
						machineType: machine,
						region:      region,
						refs:        spotRefs,
					})
				} else {
					spotDev := authoritativeDeviation(computedSpot, spotRefs)
					spotKind := ResultOK
					if math.Abs(spotDev) > tolerance {
						spotKind = ResultMismatch
					}
					results = append(results, result{
						kind:        spotKind,
						priceType:   priceKindSpot,
						machineType: machine,
						region:      region,
						computed:    computedSpot,
						refs:        spotRefs,
						deviation:   spotDev,
					})
				}
			}
		}

		for machine, odPrice := range computedEntry.OnDemand {
			if allRefNames[machine] {
				continue
			}
			kind := ResultExtraNew
			if isKnownExtra(machine) {
				kind = ResultExtra
			}
			results = append(results, result{
				kind: kind, priceType: priceKindOnDemand, machineType: machine, region: region,
				computed: odPrice,
			})
		}
	}

	return results
}

// printReport sorts results, prints actionable ones, prints a summary line, and
// returns 1 if any actionable results (MISMATCH/MISSING/EXTRA_NEW) are present.
// refLabels names each source for output; nil falls back to "ref0", "ref1", …
func printReport(results []result, refLabels []string, nRegions int, tolerance float64) int {
	slices.SortFunc(results, func(a, b result) int {
		if a.region != b.region {
			return cmp.Compare(a.region, b.region)
		}
		if a.machineType != b.machineType {
			return cmp.Compare(a.machineType, b.machineType)
		}
		return cmp.Compare(a.priceType, b.priceType)
	})

	labelFor := func(i int) string {
		if i < len(refLabels) {
			return refLabels[i]
		}
		return fmt.Sprintf("ref%d", i)
	}

	var checked, mismatches, missing, extra, extraNew, unavail, blacklisted int
	for _, r := range results {
		if r.priceType == priceKindOnDemand && (r.kind == ResultOK || r.kind == ResultMismatch) {
			checked++
		}
		switch r.kind {
		case ResultOK:
			// within tolerance — silent
		case ResultUnavail:
			unavail++
		case ResultBlacklist:
			blacklisted++
		case ResultExtra:
			extra++
		case ResultMismatch:
			mismatches++
			printResult(r, labelFor, tolerance)
		case ResultMissing:
			missing++
			printResult(r, labelFor, tolerance)
		case ResultExtraNew:
			extraNew++
			printResult(r, labelFor, tolerance)
		}
	}

	fmt.Printf("\nSummary over %d region(s): checked=%d  mismatches=%d  missing=%d  extra=%d  extra_new=%d  unavail=%d  blacklisted=%d  tolerance=%.0f%%\n",
		nRegions, checked, mismatches, missing, extra, extraNew, unavail, blacklisted, tolerance*100)

	if mismatches > 0 || missing > 0 || extraNew > 0 {
		return 1
	}
	return 0
}

func printResult(r result, labelFor func(int) string, tolerance float64) {
	computed := "n/a"
	if r.computed != 0 {
		computed = fmt.Sprintf("%.6f", r.computed)
	}
	refParts := make([]string, len(r.refs))
	for i, ref := range r.refs {
		refParts[i] = fmt.Sprintf("%s=%s", labelFor(i), fmtRef(r.computed, ref, tolerance))
	}
	fmt.Printf("%-9s %-40s %-28s %-8s computed=%-12s %s\n",
		r.kind, r.machineType, r.region, r.priceType,
		computed, strings.Join(refParts, "  "))
}

// classifyMissing returns the appropriate ResultKind for a machine that is in
// reference sources but absent from computed prices.
func classifyMissing(machine, region string, availability RegionAvailability) ResultKind {
	if av, ok := availability[region]; ok && !av[machine] {
		return ResultUnavail
	}
	return ResultMissing
}

// authoritativeDeviation returns the signed fractional deviation of computed
// relative to the first non-zero ref (the authoritative source). Returns 0 if
// no ref has a price.
func authoritativeDeviation(computed float64, refs []float64) float64 {
	for _, ref := range refs {
		if ref == 0 {
			continue
		}
		return percentDiff(computed, ref)
	}
	return 0
}

// percentDiff returns the signed fractional difference of computed relative to
// ref: positive when computed > ref, negative when computed < ref.
func percentDiff(computed, ref float64) float64 {
	if ref == 0 {
		if computed == 0 {
			return 0
		}
		return 1
	}
	return (computed - ref) / ref
}

// fmtRef formats a reference price for the output line:
//   - "n/a"              — source has no price for this machine
//   - "0.388000"         — source has a price but computed is absent (MISSING)
//   - "0.388000(ok)"     — within tolerance
//   - "0.400000(+3.1%)"  — mismatch, showing deviation from reference
func fmtRef(computed, ref, tolerance float64) string {
	if ref == 0 {
		return "n/a"
	}
	if computed == 0 {
		return fmt.Sprintf("%.6f", ref)
	}
	dev := percentDiff(computed, ref)
	if math.Abs(dev) <= tolerance {
		return fmt.Sprintf("%.6f(ok)", ref)
	}
	return fmt.Sprintf("%.6f(%+.1f%%)", ref, dev*100)
}

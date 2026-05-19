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
	"math"
	"slices"
	"testing"
)

const tol = 0.01 // 1% tolerance used throughout

func sortResults(rs []result) {
	slices.SortFunc(rs, func(a, b result) int {
		if a.region != b.region {
			return cmp.Compare(a.region, b.region)
		}
		if a.machineType != b.machineType {
			return cmp.Compare(a.machineType, b.machineType)
		}
		return cmp.Compare(a.priceType, b.priceType)
	})
}

// assertResults checks kind, priceType, and deviation for each result in order.
// Pass-through fields (region, machineType, computed, refs) are not asserted —
// they are set by buildResults directly from the inputs.
func assertResults(t *testing.T, got []result, want []result) {
	t.Helper()
	sortResults(got)
	sortResults(want)
	if len(got) != len(want) {
		t.Fatalf("len(results)=%d; want %d\n  got:  %v\n  want: %v", len(got), len(want), got, want)
	}
	for i := range got {
		g, w := got[i], want[i]
		if g.kind != w.kind || g.priceType != w.priceType || math.Abs(g.deviation-w.deviation) > 1e-12 {
			t.Errorf("result[%d]: got {kind:%s priceType:%s deviation:%g}; want {kind:%s priceType:%s deviation:%g}",
				i, g.kind, g.priceType, g.deviation, w.kind, w.priceType, w.deviation)
		}
	}
}

func TestBuildResults(t *testing.T) {
	const (
		region  = "us-central1"
		machine = "e2-standard-2"
	)

	// od builds a RegionPrices with a single on-demand price for the test region/machine.
	od := func(price float64) RegionPrices {
		return RegionPrices{region: {OnDemand: map[string]float64{machine: price}}}
	}
	// odSpot builds a RegionPrices with both on-demand and spot prices for the test region/machine.
	odSpot := func(odPrice, spotPrice float64) RegionPrices {
		return RegionPrices{region: {
			OnDemand: map[string]float64{machine: odPrice},
			Spot:     map[string]float64{machine: spotPrice},
		}}
	}

	noExtra := func(string) bool { return false }

	tests := []struct {
		name       string
		computed   RegionPrices
		refs       []RegionPrices
		avail      RegionAvailability
		knownExtra func(string) bool
		want       []result
	}{
		{
			name:     "exact on-demand match",
			computed: od(1.0),
			refs:     []RegionPrices{od(1.0)},
			want:     []result{{kind: ResultOK, priceType: priceKindOnDemand}},
		},
		{
			name:     "on-demand mismatch",
			computed: od(1.1),
			refs:     []RegionPrices{od(1.0)},
			want:     []result{{kind: ResultMismatch, priceType: priceKindOnDemand, deviation: percentDiff(1.1, 1.0)}},
		},
		{
			name:     "within tolerance is OK",
			computed: od(1.005),
			refs:     []RegionPrices{od(1.0)},
			want:     []result{{kind: ResultOK, priceType: priceKindOnDemand, deviation: percentDiff(1.005, 1.0)}},
		},
		{
			name:     "machine missing from computed",
			computed: RegionPrices{},
			refs:     []RegionPrices{od(1.0)},
			want:     []result{{kind: ResultMissing, priceType: priceKindOnDemand}},
		},
		{
			name:     "machine unavailable in GCP API",
			computed: RegionPrices{},
			refs:     []RegionPrices{od(1.0)},
			avail:    RegionAvailability{region: {machine: false}},
			want:     []result{{kind: ResultUnavail, priceType: priceKindOnDemand}},
		},
		{
			name:     "extra machine not in any ref",
			computed: RegionPrices{region: {OnDemand: map[string]float64{"new-machine": 5.0}}},
			refs:     []RegionPrices{{}},
			want:     []result{{kind: ResultExtraNew, priceType: priceKindOnDemand}},
		},
		{
			name:       "known extra machine is silent",
			computed:   RegionPrices{region: {OnDemand: map[string]float64{"known": 5.0}}},
			refs:       []RegionPrices{{}},
			knownExtra: func(m string) bool { return m == "known" },
			want:       []result{{kind: ResultExtra, priceType: priceKindOnDemand}},
		},
		{
			name:     "spot unavailable in GCP API",
			computed: od(1.0),
			refs:     []RegionPrices{odSpot(1.0, 0.3)},
			avail:    RegionAvailability{region: {machine: false}},
			want: []result{
				{kind: ResultOK, priceType: priceKindOnDemand},
				{kind: ResultUnavail, priceType: priceKindSpot},
			},
		},
		{
			name:     "spot mismatch alongside matching on-demand",
			computed: odSpot(1.0, 0.2),
			refs:     []RegionPrices{odSpot(1.0, 0.3)},
			want: []result{
				{kind: ResultOK, priceType: priceKindOnDemand},
				{kind: ResultMismatch, priceType: priceKindSpot, deviation: percentDiff(0.2, 0.3)},
			},
		},
		{
			name:     "no spot in refs — no spot result emitted",
			computed: odSpot(1.0, 0.2),
			refs:     []RegionPrices{od(1.0)},
			want:     []result{{kind: ResultOK, priceType: priceKindOnDemand}},
		},
		{
			name:     "trust hierarchy: first ref agrees — second ref disagreement ignored",
			computed: od(1.0),
			refs:     []RegionPrices{od(1.0), od(2.0)}, // authoritative, lower-trust
			want:     []result{{kind: ResultOK, priceType: priceKindOnDemand, deviation: percentDiff(1.0, 1.0)}},
		},
		{
			name:     "trust hierarchy: first ref disagrees — lower-trust agreement ignored",
			computed: od(1.5),
			refs:     []RegionPrices{od(1.0), od(1.5)}, // authoritative disagrees, lower-trust agrees
			want:     []result{{kind: ResultMismatch, priceType: priceKindOnDemand, deviation: percentDiff(1.5, 1.0)}},
		},
		{
			name:     "spot missing from computed — refs have spot data",
			computed: od(1.0),
			refs:     []RegionPrices{odSpot(1.0, 0.3)},
			want: []result{
				{kind: ResultOK, priceType: priceKindOnDemand},
				{kind: ResultMissing, priceType: priceKindSpot},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			knownExtra := tt.knownExtra
			if knownExtra == nil {
				knownExtra = noExtra
			}
			got := buildResults(tt.computed, tt.refs, tt.avail, []string{region}, knownExtra, tol)
			assertResults(t, got, tt.want)
		})
	}
}

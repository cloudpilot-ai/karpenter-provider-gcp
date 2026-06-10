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

import "testing"

func TestMachineFamily(t *testing.T) {
	for machine, want := range map[string]string{
		"c3-standard-176": "c3",
		"c4a-highmem-96":  "c4a",
		"c4d-highcpu-384": "c4d",
		"n2-standard-8":   "n2",
	} {
		if got := machineFamily(machine); got != want {
			t.Errorf("machineFamily(%q) = %q, want %q", machine, got, want)
		}
	}
}

func TestFillLSSDSpotPrices(t *testing.T) {
	gcpWeb := RegionPrices{
		"us-central1": {
			Spot: map[string]float64{
				"c3-standard-176": 1.78992,
				"c4-standard-8":   0.11111,
			},
		},
	}
	ssdRates := SSDSpotRates{
		"us-central1": {
			"all": 0.00004411,
			"c4":  0.000109452,
		},
	}
	scratchSizes := ScratchDiskSizes{
		"c3-standard-176-lssd": 12000,
		"c4-standard-8-lssd":   375,
		"h4d-standard-8-lssd":  375,
	}

	fillLSSDSpotPrices(gcpWeb, ssdRates, scratchSizes)

	spot := gcpWeb["us-central1"].Spot
	if got, want := spot["c3-standard-176-lssd"], 1.78992+12000*0.00004411; got != want {
		t.Errorf("c3 lssd spot = %v, want %v", got, want)
	}
	if got, want := spot["c4-standard-8-lssd"], 0.11111+375*0.000109452; got != want {
		t.Errorf("c4 lssd spot = %v, want %v", got, want)
	}
	if _, ok := spot["h4d-standard-8-lssd"]; ok {
		t.Errorf("h4d lssd spot should not use the generic all rate")
	}
}

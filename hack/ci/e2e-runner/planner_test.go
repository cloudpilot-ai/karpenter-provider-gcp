/*
Copyright 2025 The CloudPilot AI Authors.

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
	"reflect"
	"testing"
	"time"
)

func TestPlanWavesIsDeterministicAndReservesCapacity(t *testing.T) {
	profiles := map[string]ResourceProfile{
		"drift":        {CPUs: 2, Addresses: 1, DiskGiB: 50},
		"provisioning": {CPUs: 2, Addresses: 1, DiskGiB: 50, Workers: 3},
		"storage":      {CPUs: 2, Addresses: 1, DiskGiB: 50},
	}
	quota := QuotaSnapshot{
		CapturedAt: time.Now(), RegionalCPUs: 8, GlobalCPUs: 8, Addresses: 4, DiskGiB: 250,
		UsedRegionalCPUs: 0, UsedGlobalCPUs: 0, UsedAddresses: 0, UsedDiskGiB: 0,
		ReserveCPUs: 2, ReserveAddresses: 1, ReserveDiskGiB: 50,
	}

	got, err := planWaves([]string{"storage", "provisioning", "drift"}, profiles, quota)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]PlannedSuite{
		{{Name: "drift", Workers: 1}, {Name: "provisioning", Workers: 2}},
		{{Name: "storage", Workers: 1}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("waves = %#v, want %#v", got, want)
	}
}

func TestPlanWavesFailsClosed(t *testing.T) {
	profiles := map[string]ResourceProfile{"gpu": {CPUs: 4, Addresses: 1, DiskGiB: 50, GPUs: 1}}
	for _, quota := range []QuotaSnapshot{
		{},
		{CapturedAt: time.Now(), RegionalCPUs: 4, GlobalCPUs: 4, Addresses: 1, DiskGiB: 50, GPUs: 0},
		{CapturedAt: time.Now(), RegionalCPUs: 4, GlobalCPUs: 4, Addresses: 1, DiskGiB: 50, GPUs: 1, ReserveCPUs: 1},
		{CapturedAt: time.Now().Add(-6 * time.Minute), RegionalCPUs: 4, GlobalCPUs: 4, Addresses: 1, DiskGiB: 50, GPUs: 1},
	} {
		if _, err := planWaves([]string{"gpu"}, profiles, quota); err == nil {
			t.Fatal("expected insufficient quota error")
		}
	}
	if _, err := planWaves([]string{"missing"}, profiles, QuotaSnapshot{CapturedAt: time.Now(), RegionalCPUs: 1, GlobalCPUs: 1, Addresses: 1, DiskGiB: 1}); err == nil {
		t.Fatal("expected missing profile error")
	}
}

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
	"fmt"
	"sort"
	"time"
)

type ResourceProfile struct {
	CPUs      int
	Addresses int
	DiskGiB   int
	GPUs      int
	Workers   int
}

type QuotaSnapshot struct {
	CapturedAt       time.Time `json:"capturedAt"`
	RegionalCPUs     int       `json:"regionalCPUs"`
	GlobalCPUs       int       `json:"globalCPUs"`
	Addresses        int       `json:"addresses"`
	DiskGiB          int       `json:"diskGiB"`
	GPUs             int       `json:"gpus"`
	UsedRegionalCPUs int       `json:"usedRegionalCPUs"`
	UsedGlobalCPUs   int       `json:"usedGlobalCPUs"`
	UsedAddresses    int       `json:"usedAddresses"`
	UsedDiskGiB      int       `json:"usedDiskGiB"`
	UsedGPUs         int       `json:"usedGPUs"`
	ReserveCPUs      int       `json:"reserveCPUs"`
	ReserveAddresses int       `json:"reserveAddresses"`
	ReserveDiskGiB   int       `json:"reserveDiskGiB"`
	ReserveGPUs      int       `json:"reserveGPUs"`
}

type PlannedSuite struct {
	Name    string `json:"name"`
	Workers int    `json:"workers"`
}

type resources struct{ cpu, addresses, disk, gpu int }

func planWaves(suites []string, profiles map[string]ResourceProfile, quota QuotaSnapshot) ([][]PlannedSuite, error) {
	if quota.CapturedAt.IsZero() || time.Since(quota.CapturedAt) > 5*time.Minute {
		return nil, fmt.Errorf("quota snapshot is missing or stale")
	}
	if quota.RegionalCPUs <= 0 || quota.GlobalCPUs <= 0 || quota.Addresses <= 0 || quota.DiskGiB <= 0 {
		return nil, fmt.Errorf("quota snapshot contains unknown or zero capacity")
	}
	available := resources{
		cpu:       min(quota.RegionalCPUs-quota.UsedRegionalCPUs, quota.GlobalCPUs-quota.UsedGlobalCPUs) - quota.ReserveCPUs,
		addresses: quota.Addresses - quota.UsedAddresses - quota.ReserveAddresses,
		disk:      quota.DiskGiB - quota.UsedDiskGiB - quota.ReserveDiskGiB,
		gpu:       quota.GPUs - quota.UsedGPUs - quota.ReserveGPUs,
	}
	if available.cpu <= 0 || available.addresses <= 0 || available.disk <= 0 || available.gpu < 0 {
		return nil, fmt.Errorf("quota leaves no safe execution capacity")
	}

	pending := append([]string(nil), suites...)
	sort.Strings(pending)
	var waves [][]PlannedSuite
	for len(pending) > 0 {
		remaining := available
		wave := make([]PlannedSuite, 0, len(pending))
		next := make([]string, 0, len(pending))
		for _, suite := range pending {
			profile, ok := profiles[suite]
			if !ok {
				return nil, fmt.Errorf("missing resource profile for suite %q", suite)
			}
			workers := maxWorkers(profile, remaining)
			if workers == 0 {
				next = append(next, suite)
				continue
			}
			wave = append(wave, PlannedSuite{Name: suite, Workers: workers})
			remaining = subtract(remaining, profile, workers)
		}
		if len(wave) == 0 {
			return nil, fmt.Errorf("quota cannot safely run suite %q", pending[0])
		}
		waves = append(waves, wave)
		pending = next
	}
	return waves, nil
}

func maxWorkers(profile ResourceProfile, available resources) int {
	if profile.CPUs <= 0 || profile.Addresses <= 0 || profile.DiskGiB <= 0 || profile.GPUs < 0 {
		return 0
	}
	workers := profile.Workers
	if workers == 0 {
		workers = 1
	}
	workers = min(workers, available.cpu/profile.CPUs, available.addresses/profile.Addresses, available.disk/profile.DiskGiB)
	if profile.GPUs > 0 {
		workers = min(workers, available.gpu/profile.GPUs)
	}
	return workers
}

func subtract(available resources, profile ResourceProfile, workers int) resources {
	return resources{
		cpu:       available.cpu - profile.CPUs*workers,
		addresses: available.addresses - profile.Addresses*workers,
		disk:      available.disk - profile.DiskGiB*workers,
		gpu:       available.gpu - profile.GPUs*workers,
	}
}

func min(values ...int) int {
	result := values[0]
	for _, value := range values[1:] {
		if value < result {
			result = value
		}
	}
	return result
}

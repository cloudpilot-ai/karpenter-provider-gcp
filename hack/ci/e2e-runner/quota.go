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
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

type computeQuota struct {
	Metric string  `json:"metric"`
	Limit  float64 `json:"limit"`
	Usage  float64 `json:"usage"`
}

type quotaDescription struct {
	Quotas []computeQuota `json:"quotas"`
}

var runGcloud = func(args ...string) ([]byte, error) {
	return exec.Command("gcloud", args...).Output()
}

func discoverQuota(project, region string) (QuotaSnapshot, error) {
	regional, err := gcloudQuotas("compute", "regions", "describe", region, "--project", project, "--format=json(quotas)")
	if err != nil {
		return QuotaSnapshot{}, fmt.Errorf("discover regional quota: %w", err)
	}
	global, err := gcloudQuotas("compute", "project-info", "describe", "--project", project, "--format=json(quotas)")
	if err != nil {
		return QuotaSnapshot{}, fmt.Errorf("discover global quota: %w", err)
	}

	quota := QuotaSnapshot{CapturedAt: time.Now().UTC()}
	var ok bool
	if quota.RegionalCPUs, quota.UsedRegionalCPUs, ok = quotaValue(regional, "CPUS"); !ok {
		return QuotaSnapshot{}, fmt.Errorf("regional CPUS quota is unavailable")
	}
	if quota.Addresses, quota.UsedAddresses, ok = quotaValue(regional, "IN_USE_ADDRESSES"); !ok {
		return QuotaSnapshot{}, fmt.Errorf("regional IN_USE_ADDRESSES quota is unavailable")
	}
	if quota.DiskGiB, quota.UsedDiskGiB, ok = quotaValue(regional, "SSD_TOTAL_GB"); !ok {
		return QuotaSnapshot{}, fmt.Errorf("regional SSD_TOTAL_GB quota is unavailable")
	}
	if quota.GlobalCPUs, quota.UsedGlobalCPUs, ok = quotaValue(global, "CPUS_ALL_REGIONS"); !ok {
		return QuotaSnapshot{}, fmt.Errorf("global CPUS_ALL_REGIONS quota is unavailable")
	}
	// GPU quota is intentionally optional: a zero GPU capacity is valid for non-GPU presets.
	quota.GPUs, quota.UsedGPUs, _ = quotaValue(regional, "NVIDIA_L4_GPUS")
	// Reserve one ordinary node for teardown/replacement overlap before scheduling waves.
	quota.ReserveCPUs = 2
	quota.ReserveAddresses = 1
	quota.ReserveDiskGiB = 50
	return quota, nil
}

func gcloudQuotas(args ...string) ([]computeQuota, error) {
	data, err := runGcloud(args...)
	if err != nil {
		return nil, err
	}
	var description quotaDescription
	if err := json.Unmarshal(data, &description); err != nil {
		return nil, fmt.Errorf("decode gcloud quota response: %w", err)
	}
	return description.Quotas, nil
}

func quotaValue(quotas []computeQuota, metric string) (limit, usage int, found bool) {
	for _, quota := range quotas {
		if quota.Metric == metric {
			return int(quota.Limit), int(quota.Usage), true
		}
	}
	return 0, 0, false
}

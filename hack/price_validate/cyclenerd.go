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
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const cyclenerdURL = "https://gcloud-compute.com/machine-types-regions.csv"

func getCyclenerdPrices(ctx context.Context, workDir string, noCache bool, cacheTTL time.Duration) (RegionPrices, error) {
	return loadOrFetch(filepath.Join(workDir, "cyclenerd.json"), noCache, cacheTTL, func() (RegionPrices, error) {
		return fetchCyclenerdPrices(ctx)
	})
}

func fetchCyclenerdPrices(ctx context.Context) (RegionPrices, error) {
	fmt.Println("  cyclenerd: fetching CSV from gcloud-compute.com...")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cyclenerdURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d fetching Cyclenerd CSV", resp.StatusCode)
	}
	return parseCyclenerdCSV(resp.Body)
}

func parseCyclenerdCSV(r io.Reader) (RegionPrices, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1 // variable column count across CSV versions

	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("reading CSV header: %w", err)
	}

	col := func(name string) int {
		for i, h := range header {
			if strings.EqualFold(h, name) {
				return i
			}
		}
		return -1
	}
	nameCol, regionCol, onDemandCol, spotCol :=
		col("name"), col("region"), col("hour"), col("hourSpot")
	if nameCol < 0 || regionCol < 0 || onDemandCol < 0 || spotCol < 0 {
		return nil, fmt.Errorf("CSV missing required columns (name/region/hour/hourSpot); got: %v", header)
	}

	result := make(RegionPrices)
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading CSV: %w", err)
		}
		if len(rec) <= max(nameCol, regionCol, onDemandCol, spotCol) {
			continue
		}
		onDemand, err := strconv.ParseFloat(rec[onDemandCol], 64)
		if err != nil || onDemand == 0 {
			continue
		}
		region, machine := rec[regionCol], rec[nameCol]
		if isNonMachineTypeEntry(machine) {
			continue
		}
		entry := result[region]
		if entry.OnDemand == nil {
			entry.OnDemand = make(map[string]float64)
			entry.Spot = make(map[string]float64)
		}
		entry.OnDemand[machine] = onDemand
		if spotPrice, err := strconv.ParseFloat(rec[spotCol], 64); err == nil && spotPrice > 0 {
			entry.Spot[machine] = spotPrice
		}
		result[region] = entry
	}
	return result, nil
}

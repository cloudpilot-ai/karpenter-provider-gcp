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
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/pricing/instanceprice"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

type RegionEntry = instanceprice.Prices

type RegionPrices map[string]RegionEntry

type RegionAvailability map[string]map[string]bool

type priceFile struct {
	SavedAt *time.Time   `json:"saved_at,omitempty"`
	Prices  RegionPrices `json:"prices"`
}

func readPriceFile(path string) (*priceFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var priceData priceFile
	if err := json.NewDecoder(f).Decode(&priceData); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", path, err)
	}
	return &priceData, nil
}

// writePriceFile serialises prices to path. Pass a non-nil savedAt to embed a
// timestamp (used for TTL cache files); pass nil to omit it (used for the
// committed output file so that git diff only fires on actual price changes).
func writePriceFile(path string, prices RegionPrices, savedAt *time.Time) error {
	data, err := json.Marshal(&priceFile{SavedAt: savedAt, Prices: prices})
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func loadOrFetch(path string, noCache bool, ttl time.Duration, fetch func() (RegionPrices, error)) (RegionPrices, error) {
	name := filepath.Base(path)
	if !noCache {
		if pf, err := readPriceFile(path); err == nil && pf.SavedAt != nil {
			if age := time.Since(*pf.SavedAt); age < ttl {
				fmt.Printf("  %-14s using cache (%.1fh old)\n", name+":", age.Hours())
				return pf.Prices, nil
			}
		}
	}
	prices, err := fetch()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if err := writePriceFile(path, prices, &now); err != nil {
		return nil, fmt.Errorf("saving %s: %w", name, err)
	}
	fmt.Printf("  %-14s %d regions\n", name+":", len(prices))
	return prices, nil
}

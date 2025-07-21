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

package pricing

import (
	"context"
	_ "embed"
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	utilsobject "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils/object"
)

//go:embed initial-on-demand-prices.json
var initialOnDemandPricesData []byte

const (
	// TODO: Get rid of 3rd party API for pricing: https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/33
	pricingCSVURL = "https://gcloud-compute.com/machine-types-regions.csv"
	// Default timeout for downloading the CSV
	pricingCSVTimeout = 24 * time.Hour
	// Price cache key
	pricingCSVCacheKey = "pricing-csv"
)

type Provider interface {
	LivenessProbe(*http.Request) error
	InstanceTypes() []string
	OnDemandPrice(string) (float64, bool)
	SpotPrice(string, string) (float64, bool)
	UpdatePrices(context.Context) error
}

type pricesStorage = map[string]float64

type DefaultProvider struct {
	region string

	muOnDemand     sync.RWMutex
	onDemandPrices pricesStorage
	muSpot         sync.RWMutex
	spotPrices     pricesStorage
}

func NewDefaultProvider(ctx context.Context, region string) (*DefaultProvider, error) {
	p := &DefaultProvider{
		region:         region,
		onDemandPrices: make(pricesStorage),
		spotPrices:     make(pricesStorage),
	}

	// sets the pricing data from the static default state for the provider
	err := p.Reset()
	return p, err
}

func (p *DefaultProvider) Reset() error {
	p.muOnDemand.Lock()
	defer p.muOnDemand.Unlock()

	// Parse the JSON data
	parsedJSON := *utilsobject.JSONUnmarshal[map[string]pricesStorage](initialOnDemandPricesData)
	// Read prices for the region
	p.onDemandPrices = parsedJSON[p.region]
	if len(p.onDemandPrices) == 0 {
		return fmt.Errorf("no initial prices found for region %s", p.region)
	}

	log.FromContext(context.TODO()).Info("Loaded initial on-demand prices", "region", p.region, "count", len(p.onDemandPrices))
	return nil
}

func (p *DefaultProvider) LivenessProbe(_ *http.Request) error {
	// ensure we don't deadlock and nolint for the empty critical section
	p.muOnDemand.Lock()
	p.muSpot.Lock()
	//nolint: staticcheck
	p.muOnDemand.Unlock()
	p.muSpot.Unlock()
	return nil
}

func (p *DefaultProvider) InstanceTypes() []string {
	p.muOnDemand.RLock()
	defer p.muOnDemand.RUnlock()

	types := make([]string, len(p.onDemandPrices))
	for t := range p.onDemandPrices {
		types = append(types, t)
	}
	return types
}

func (p *DefaultProvider) OnDemandPrice(instanceType string) (float64, bool) {
	p.muOnDemand.RLock()
	defer p.muOnDemand.RUnlock()

	price, ok := p.onDemandPrices[instanceType]
	return price, ok
}

// Zone parameter is ignored, cause in GCP prices are regional
func (p *DefaultProvider) SpotPrice(instanceType string, zone string) (float64, bool) {
	p.muSpot.RLock()
	defer p.muSpot.RUnlock()

	price, ok := p.spotPrices[instanceType]
	return price, ok
}

func (p *DefaultProvider) downloadCSV(ctx context.Context) ([][]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pricingCSVURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	// Download the CSV file
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading CSV: %w", err)
	}
	defer resp.Body.Close()

	// Read all CSV data
	reader := csv.NewReader(resp.Body)
	reader.Comma = ','
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("reading CSV: %w", err)
	}

	if len(records) < 2 {
		return nil, fmt.Errorf("CSV file is empty or has no data")
	}

	return records, nil
}

func (p *DefaultProvider) UpdatePrices(ctx context.Context) error {
	newRecords, err := p.downloadCSV(ctx)
	if err != nil {
		return err
	}
	if len(newRecords) < 2 {
		log.FromContext(ctx).Error(err, "CSV file is empty or has no data")
		return fmt.Errorf("CSV file is empty or has no data")
	}

	onDemandPrices, spotPrices, err := resolvePrice(newRecords, p.region)
	if err != nil {
		log.FromContext(ctx).Error(err, "Error resolving prices")
		return err
	}

	if len(onDemandPrices) == 0 {
		return fmt.Errorf("no prices retrieved during update")
	}

	p.muOnDemand.Lock()
	p.muSpot.Lock()
	defer p.muOnDemand.Unlock()
	defer p.muSpot.Unlock()

	p.onDemandPrices = onDemandPrices
	p.spotPrices = spotPrices
	return nil
}

func resolvePrice(newRecords [][]string, region string) (pricesStorage, pricesStorage, error) {
	// Find required columns
	header := newRecords[0]
	var onDemandPriceColumn, spotPriceColumn, regionCol, nameCol int
	for i, col := range header {
		switch col {
		case "hour":
			onDemandPriceColumn = i
		case "hourSpot":
			spotPriceColumn = i
		case "region":
			regionCol = i
		case "name":
			nameCol = i
		}
	}

	// Process the data
	onDemandPrices := make(pricesStorage)
	spotPrices := make(pricesStorage)

	for _, record := range newRecords[1:] {
		// Skip records that don't match our region
		if record[regionCol] != region {
			continue
		}

		machineType := record[nameCol]
		onDemandPriceStr := record[onDemandPriceColumn]
		spotPriceStr := record[spotPriceColumn]

		onDemandPrice, err := strconv.ParseFloat(onDemandPriceStr, 64)
		if err != nil {
			return nil, nil, fmt.Errorf("error parsing on-demand price: %w", err)
		}

		spotPrice, err := strconv.ParseFloat(spotPriceStr, 64)
		if err != nil {
			return nil, nil, fmt.Errorf("error parsing spot price: %w", err)
		}

		onDemandPrices[machineType] = onDemandPrice
		spotPrices[machineType] = spotPrice
	}

	return onDemandPrices, spotPrices, nil
}

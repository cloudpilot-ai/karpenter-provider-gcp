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

	utilsobject "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils/object"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

//go:embed initial-on-demand-prices.json
var initialOnDemandPricesData []byte

const (
	// TODO: Get rid of 3rd party API for pricing: https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/33
	pricingCSVURL = "https://gcloud-compute.com/machine-types-regions.csv"
)

type Provider interface {
	LivenessProbe(*http.Request) error
	InstanceTypes() []string
	OnDemandPrice(string) (float64, bool)
	SpotPrice(string, string) (float64, bool)
	UpdateOnDemandPricing(context.Context) error
	UpdateSpotPricing(context.Context) error
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

	log.FromContext(context.TODO()).Info("Loaded initial on-demand prices", "region", p.region, "prices", p.onDemandPrices, "count", len(p.onDemandPrices))
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

func (p *DefaultProvider) updatePrices(ctx context.Context, priceColumn string, storage *pricesStorage) error {
	records, err := p.downloadCSV(ctx)
	if err != nil {
		return err
	}

	// Find required columns
	header := records[0]
	var priceCol, regionCol, nameCol int
	for i, col := range header {
		switch col {
		case priceColumn:
			priceCol = i
		case "region":
			regionCol = i
		case "name":
			nameCol = i
		}
	}

	// Process the data
	newPrices := make(pricesStorage)

	for _, record := range records[1:] {
		// Skip records that don't match our region
		if record[regionCol] != p.region {
			continue
		}

		machineType := record[nameCol]
		priceStr := record[priceCol]

		price, err := strconv.ParseFloat(priceStr, 64)
		if err != nil {
			log.FromContext(ctx).Error(err, "Error parsing price", "machineType", machineType, "price", priceStr)
			continue
		}

		newPrices[machineType] = price
	}

	if len(newPrices) == 0 {
		return fmt.Errorf("no prices retrieved during update")
	}

	*storage = newPrices
	return nil
}

func (p *DefaultProvider) UpdateOnDemandPricing(ctx context.Context) error {
	p.muOnDemand.Lock()
	defer p.muOnDemand.Unlock()

	return p.updatePrices(ctx, "hour", &p.onDemandPrices)
}

func (p *DefaultProvider) UpdateSpotPricing(ctx context.Context) error {
	p.muSpot.Lock()
	defer p.muSpot.Unlock()

	return p.updatePrices(ctx, "hourSpot", &p.spotPrices)
}

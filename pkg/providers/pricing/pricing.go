/*
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
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"k8s.io/klog/v2"
)

const (
	initialOnDemandPricesFile = "initial-on-demand-prices.json"
)

type Provider interface {
	LivenessProbe(*http.Request) error
	InstanceTypes() []string
	OnDemandPrice(string) (float64, bool)
	SpotPrice(string, string) (float64, bool)
	UpdateOnDemandPricing(context.Context) error
	UpdateSpotPricing(context.Context) error
}

type DefaultProvider struct {
	muPriceLastUpdatedTimestamp sync.RWMutex
	muOnDemand                  sync.RWMutex
	muSpot                      sync.RWMutex
	region                      string
	prices                      map[string]map[string]float64
}

func NewDefaultProvider(ctx context.Context, region string) *DefaultProvider {
	p := &DefaultProvider{
		region: region,
		prices: make(map[string]map[string]float64),
	}

	if err := p.loadInitialPrices(); err != nil {
		klog.Errorf("Failed to load initial prices: %v", err)
	}

	return p
}

func (p *DefaultProvider) loadInitialPrices() error {
	p.muOnDemand.Lock()
	defer p.muOnDemand.Unlock()

	// Get the directory of the current file
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		return fmt.Errorf("failed to get current file path")
	}
	dir := filepath.Dir(filename)

	// Read the JSON file
	data, err := os.ReadFile(filepath.Join(dir, initialOnDemandPricesFile))
	if err != nil {
		return fmt.Errorf("reading initial prices file: %w", err)
	}

	// Parse the JSON data
	if err := json.Unmarshal(data, &p.prices); err != nil {
		return fmt.Errorf("parsing initial prices: %w", err)
	}

	klog.V(2).Infof("Loaded prices for %d regions", len(p.prices))
	return nil
}

func (p *DefaultProvider) LivenessProbe(_ *http.Request) error {
	return nil
}

func (p *DefaultProvider) InstanceTypes() []string {
	p.muOnDemand.RLock()
	defer p.muOnDemand.RUnlock()

	regionPrices, ok := p.prices[p.region]
	if !ok {
		return nil
	}

	types := make([]string, 0, len(regionPrices))
	for t := range regionPrices {
		types = append(types, t)
	}
	return types
}

func (p *DefaultProvider) OnDemandPrice(instanceType string) (float64, bool) {
	p.muOnDemand.RLock()
	defer p.muOnDemand.RUnlock()

	regionPrices, ok := p.prices[p.region]
	if !ok {
		fmt.Println(p.prices)
		return 0, false
	}

	price, ok := regionPrices[instanceType]
	return price, ok
}

func (p *DefaultProvider) SpotPrice(instanceType string, zone string) (float64, bool) {
	// Currently, we don't have spot price information
	return 0, false
}

func (p *DefaultProvider) UpdateOnDemandPricing(ctx context.Context) error {
	// For now, we only use static pricing data
	return nil
}

func (p *DefaultProvider) UpdateSpotPricing(ctx context.Context) error {
	// Currently, we don't have spot price information
	return nil
}

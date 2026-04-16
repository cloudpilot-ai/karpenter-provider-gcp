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
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/pricing/instanceprice"
)

//go:embed initial-prices.json
var initialPricesData []byte

type Provider interface {
	LivenessProbe(*http.Request) error
	InstanceTypes() []string
	OnDemandPrice(string) (float64, bool)
	SpotPrice(string, string) (float64, bool)
	UpdatePrices(context.Context) error
}

// PricingClient is the interface used to fetch live prices from the GCP Billing API.
// Pass nil to NewDefaultProvider to disable live updates (embedded prices only).
type PricingClient interface {
	FetchRegionPrices(ctx context.Context, region string) (instanceprice.Prices, error)
}

type pricesStorage = map[string]float64

// initialPricesFile matches the price_validate computed.json / update-pricing CI
// output format so that make update-pricing can copy that file directly.
type initialPricesFile struct {
	Prices map[string]regionEntry `json:"prices"`
}

type regionEntry = instanceprice.Prices

type DefaultProvider struct {
	region    string
	client PricingClient

	mu             sync.RWMutex
	onDemandPrices pricesStorage
	spotPrices     pricesStorage
}

// NewDefaultProvider creates a pricing provider for region. Pass a non-nil client to
// enable live price updates via UpdatePrices; pass nil to rely on embedded prices only.
func NewDefaultProvider(ctx context.Context, client PricingClient, region string) (*DefaultProvider, error) {
	p := &DefaultProvider{
		region:         region,
		client:      client,
		onDemandPrices: make(pricesStorage),
		spotPrices:     make(pricesStorage),
	}
	if err := p.Reset(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *DefaultProvider) Reset() error {
	var data initialPricesFile
	if err := json.Unmarshal(initialPricesData, &data); err != nil {
		return fmt.Errorf("parsing initial-prices.json: %w", err)
	}

	regionPrices, ok := data.Prices[p.region]
	if !ok || len(regionPrices.OnDemand) == 0 {
		return fmt.Errorf("no initial prices found for region %s", p.region)
	}

	p.mu.Lock()
	p.onDemandPrices = regionPrices.OnDemand
	p.spotPrices = make(pricesStorage)
	p.mu.Unlock()
	return nil
}

func (p *DefaultProvider) LivenessProbe(_ *http.Request) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.onDemandPrices) == 0 {
		return fmt.Errorf("pricing provider has no on-demand prices loaded")
	}
	return nil
}

func (p *DefaultProvider) InstanceTypes() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return slices.Collect(maps.Keys(p.onDemandPrices))
}

func (p *DefaultProvider) OnDemandPrice(instanceType string) (float64, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	price, ok := p.onDemandPrices[instanceType]
	return price, ok
}

// SpotPrice ignores zone (GCP prices are regional).
// Falls back to 40% of on-demand when no spot price is known.
func (p *DefaultProvider) SpotPrice(instanceType string, _ string) (float64, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if price, ok := p.spotPrices[instanceType]; ok {
		return price, true
	}
	if odPrice, ok := p.onDemandPrices[instanceType]; ok {
		return odPrice * 0.4, true
	}
	return 0, false
}

func (p *DefaultProvider) UpdatePrices(ctx context.Context) error {
	if p.client == nil {
		return fmt.Errorf("client is nil: cannot update prices")
	}

	prices, err := p.client.FetchRegionPrices(ctx, p.region)
	if err != nil {
		return fmt.Errorf("fetching prices for %s: %w", p.region, err)
	}
	if len(prices.OnDemand) == 0 {
		return fmt.Errorf("no prices retrieved for region %s", p.region)
	}

	p.mu.Lock()
	p.onDemandPrices = prices.OnDemand
	p.spotPrices = prices.Spot
	p.mu.Unlock()

	log.FromContext(ctx).Info("Updated prices", "region", p.region, "onDemand", len(prices.OnDemand), "spot", len(prices.Spot))
	return nil
}

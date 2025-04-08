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
	"net/http"
	"sync"
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
}

func NewDefaultProvider(ctx context.Context, region string) *DefaultProvider {
	// Implement me
	return nil
}

func (p *DefaultProvider) LivenessProbe(_ *http.Request) error {
	// Implement me
	p.muOnDemand.Lock()
	p.muSpot.Lock()
	p.muPriceLastUpdatedTimestamp.Lock()
	//nolint: staticcheck
	p.muOnDemand.Unlock()
	p.muSpot.Unlock()
	p.muPriceLastUpdatedTimestamp.Unlock()
	return nil
}

func (p *DefaultProvider) InstanceTypes() []string {
	// Implement me
	p.muOnDemand.RLock()
	p.muSpot.RLock()
	defer p.muOnDemand.RUnlock()
	defer p.muSpot.RUnlock()
	return nil
}

func (p *DefaultProvider) OnDemandPrice(instanceType string) (float64, bool) {
	// Implement me
	p.muOnDemand.RLock()
	defer p.muOnDemand.RUnlock()
	return -1.0, false
}

func (p *DefaultProvider) SpotPrice(instanceType string, zone string) (float64, bool) {
	// Implement me
	p.muSpot.RLock()
	defer p.muSpot.RUnlock()
	return -1.0, false
}

func (p *DefaultProvider) UpdateOnDemandPricing(ctx context.Context) error {
	// Implement me
	p.muOnDemand.RLock()
	defer p.muOnDemand.RUnlock()
	return nil
}

func (p *DefaultProvider) UpdateSpotPricing(ctx context.Context) error {
	// Implement me
	p.muSpot.RLock()
	defer p.muSpot.RUnlock()
	return nil
}

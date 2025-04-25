/*
Copyright 2024 The CloudPilot AI Authors.

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

package pricing_test

import (
	"context"
	"testing"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/pricing"
)

func TestDefaultProvider_OnDemandPrice(t *testing.T) {
	// Initialize the provider with europe-west4 region
	provider := pricing.NewDefaultProvider(context.Background(), "europe-west4")
	provider.UpdateOnDemandPricing(context.Background())

	// Test getting price for e2-standard-32
	price, found := provider.OnDemandPrice("e2-standard-32")
	if !found {
		t.Fatal("Failed to find price for e2-standard-32")
	}

	t.Logf("On-demand price for e2-standard-32 in europe-west4: $%.4f per hour", price)

	// Test getting price for a non-existent instance type
	_, found = provider.OnDemandPrice("non-existent-type")
	if found {
		t.Error("Expected to not find price for non-existent instance type")
	}
}

func TestDefaultProvider_InstanceTypes(t *testing.T) {
	// Initialize the provider with europe-west4 region
	provider := pricing.NewDefaultProvider(context.Background(), "europe-west4")
	provider.UpdateOnDemandPricing(context.Background())

	// Get all instance types
	types := provider.InstanceTypes()
	if len(types) == 0 {
		t.Fatal("No instance types found")
	}

	t.Logf("Found %d instance types in europe-west4", len(types))

	// Check if e2-standard-32 is in the list
	found := false
	for _, instanceType := range types {
		if instanceType == "e2-standard-32" {
			found = true
			break
		}
	}

	if !found {
		t.Error("e2-standard-32 not found in instance types list")
	}
}

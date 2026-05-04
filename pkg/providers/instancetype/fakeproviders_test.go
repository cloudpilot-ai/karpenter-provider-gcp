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

package instancetype

import (
	"context"
	"net/http"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/patrickmn/go-cache"
	containerv1 "google.golang.org/api/container/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/auth"
	pkgcache "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/cache"
)

// fakeGKEProvider returns a fixed zone list without making GCP API calls.
type fakeGKEProvider struct{}

func (f *fakeGKEProvider) ResolveClusterZones(_ context.Context) ([]string, error) {
	return []string{"us-central1-a"}, nil
}

func (f *fakeGKEProvider) GetClusterConfig(_ context.Context) (*containerv1.Cluster, error) {
	return &containerv1.Cluster{
		NetworkConfig: &containerv1.NetworkConfig{
			Network:    "projects/test-project/global/networks/default",
			Subnetwork: "projects/test-project/regions/us-central1/subnetworks/default",
		},
	}, nil
}

// fakePricingProvider returns a fixed price for every instance type without
// making GCP API calls.
type fakePricingProvider struct{}

func (f *fakePricingProvider) LivenessProbe(_ *http.Request) error    { return nil }
func (f *fakePricingProvider) InstanceTypes() []string                { return nil }
func (f *fakePricingProvider) OnDemandPrice(_ string) (float64, bool) { return 1.0, true }
func (f *fakePricingProvider) SpotPrice(_, _ string) (float64, bool)  { return 0.5, true }
func (f *fakePricingProvider) UpdatePrices(_ context.Context) error   { return nil }

// newTestProvider builds a DefaultProvider wired with fakes and pre-populated
// with one machine type so that List can return results without network calls.
func newTestProvider() *DefaultProvider {
	mt := &computepb.MachineType{
		Name:      aws.String("n2-standard-4"),
		GuestCpus: aws.Int32(4),
		MemoryMb:  aws.Int32(16384),
	}
	return &DefaultProvider{
		authOptions:       &auth.Credential{Region: "us-central1"},
		pricingProvider:   &fakePricingProvider{},
		gkeProvider:       &fakeGKEProvider{},
		instanceTypesInfo: []*computepb.MachineType{mt},
		instanceTypesOfferings: map[string]sets.Set[string]{
			"n2-standard-4": sets.New("us-central1-a"),
		},
		unavailableOfferings: pkgcache.NewUnavailableOfferings(),
		instanceTypesCache:   cache.New(InstanceTypesCacheTTL, pkgcache.DefaultCleanupInterval),
		cm:                   pretty.NewChangeMonitor(),
	}
}

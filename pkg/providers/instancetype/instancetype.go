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

package instancetype

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/patrickmn/go-cache"
	"google.golang.org/api/iterator"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/auth"
	pkgcache "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/cache"
)

const (
	InstanceTypesCacheKey = "gce-instancetypes"
	InstanceTypesCacheTTL = 23 * time.Hour
)

type Provider interface {
	LivenessProbe(*http.Request) error
	List(context.Context, *v1alpha1.KubeletConfiguration, *v1alpha1.GCENodeClass) ([]*cloudprovider.InstanceType, error)
	UpdateInstanceTypes(ctx context.Context) error
	UpdateInstanceTypeOfferings(ctx context.Context) error
}

type DefaultProvider struct {
	authOptions   *auth.Credential
	vmTypesClient *compute.MachineTypesClient

	muCache sync.Mutex
	cache   *cache.Cache
}

func NewProvider(region string, authOptions *auth.Credential) (*DefaultProvider, error) {
	vmTypesClient, err := compute.NewMachineTypesRESTClient(context.Background(), authOptions.CredentialOptions)
	if err != nil {
		return nil, err
	}
	return &DefaultProvider{
		authOptions:   authOptions,
		vmTypesClient: vmTypesClient,
		cache:         cache.New(InstanceTypesCacheTTL, pkgcache.DefaultCleanupInterval),
	}, nil
}

func (p *DefaultProvider) LivenessProbe(*http.Request) error {
	// TODO: Implement me
	return nil
}

func (p *DefaultProvider) List(ctx context.Context, kc *v1alpha1.KubeletConfiguration, nodeClass *v1alpha1.GCENodeClass) ([]*cloudprovider.InstanceType, error) {
	// TODO: Implement me
	p.muCache.Lock()
	defer p.muCache.Unlock()

	return nil, nil
}

func (p *DefaultProvider) getInstanceTypes(ctx context.Context) ([]*computepb.MachineType, error) {
	p.muCache.Lock()
	defer p.muCache.Unlock()

	if cached, ok := p.cache.Get(InstanceTypesCacheKey); ok {
		return cached.([]*computepb.MachineType), nil
	}

	vmFilter := fmt.Sprintf("(zone eq .*%s-.*)", p.authOptions.Region)
	req := &computepb.AggregatedListMachineTypesRequest{
		Project: p.authOptions.ProjectID,
		Filter:  &vmFilter,
	}

	it := p.vmTypesClient.AggregatedList(ctx, req)

	var vmTypes []*computepb.MachineType
	for {
		resp, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}

		if err != nil {
			return nil, err
		}

		vmTypes = append(vmTypes, resp.Value.MachineTypes...)
	}

	p.cache.SetDefault(InstanceTypesCacheKey, vmTypes)
	return vmTypes, nil
}

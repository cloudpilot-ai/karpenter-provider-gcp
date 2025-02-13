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

package instance

import (
	"context"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

type Provider interface {
	Create(context.Context, *v1alpha1.GCENodeClass, *karpv1.NodeClaim, []*cloudprovider.InstanceType) (*Instance, error)
	Get(context.Context, string) (*Instance, error)
	List(context.Context) ([]*Instance, error)
	Delete(context.Context, string) error
	CreateTags(context.Context, string, map[string]string) error
}

type DefaultProvider struct {
	region string
}

func NewProvider(region string) *DefaultProvider {
	return &DefaultProvider{
		region: region,
	}
}

func (p *DefaultProvider) Create(ctx context.Context, nodeClass *v1alpha1.GCENodeClass, nodeClaim *karpv1.NodeClaim, instanceTypes []*cloudprovider.InstanceType) (*Instance, error) {
	// TODO: Implement me
	return nil, nil
}

func (p *DefaultProvider) Get(ctx context.Context, providerID string) (*Instance, error) {
	// TODO: Implement me
	return nil, nil
}

func (p *DefaultProvider) List(ctx context.Context) ([]*Instance, error) {
	// TODO: Implement me
	return nil, nil
}

func (p *DefaultProvider) Delete(ctx context.Context, providerID string) error {
	// TODO: Implement me
	return nil
}

func (p *DefaultProvider) CreateTags(ctx context.Context, providerID string, tags map[string]string) error {
	// TODO: Implement me
	return nil
}

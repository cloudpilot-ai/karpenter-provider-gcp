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
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

type Provider interface {
	LivenessProbe(*http.Request) error
	List(context.Context, *v1alpha1.KubeletConfiguration, *v1alpha1.GCENodeClass) ([]*cloudprovider.InstanceType, error)
	UpdateInstanceTypes(ctx context.Context) error
	UpdateInstanceTypeOfferings(ctx context.Context) error
}

type DefaultProvider struct {
	region     string
	kubeClient client.Client
}

func NewProvider(kubeClient client.Client, region string) *DefaultProvider {
	return &DefaultProvider{
		kubeClient: kubeClient,
		region:     region,
	}
}

func (p *DefaultProvider) LivenessProbe(*http.Request) error {
	// TODO: Implement me
	return nil
}

func (p *DefaultProvider) List(ctx context.Context, kubeletConfiguration *v1alpha1.KubeletConfiguration, nodeClass *v1alpha1.GCENodeClass) ([]*cloudprovider.InstanceType, error) {
	// TODO: Implement me
	return nil, nil
}

func (p *DefaultProvider) UpdateInstanceTypes(ctx context.Context) error {
	// TODO: Implement me
	return nil
}

func (p *DefaultProvider) UpdateInstanceTypeOfferings(ctx context.Context) error {
	// TODO: Implement me
	return nil
}

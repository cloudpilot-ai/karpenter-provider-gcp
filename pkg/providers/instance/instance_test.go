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

package instance

import (
	"testing"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
)

func TestMergeInstanceTagsPreservesTemplateAndAddsNetworkTags(t *testing.T) {
	base := &compute.Tags{Items: []string{"gke-default", "existing"}, Fingerprint: "fp"}

	merged := mergeInstanceTags(base, []string{"existing", "custom"})

	require.NotNil(t, merged)
	require.Equal(t, []string{"gke-default", "existing", "existing", "custom"}, merged.Items)
	require.Equal(t, "fp", merged.Fingerprint)
	require.Equal(t, []string{"gke-default", "existing"}, base.Items, "base tags should not be mutated")
	require.NotSame(t, base, merged)
}

func TestMergeInstanceTagsHandlesNilTemplate(t *testing.T) {
	merged := mergeInstanceTags(nil, []string{"tag-one", "tag-two"})

	require.NotNil(t, merged)
	require.Equal(t, []string{"tag-one", "tag-two"}, merged.Items)
	require.Empty(t, merged.Fingerprint)
}

func TestMergeInstanceTagsReturnsNilWhenNoTags(t *testing.T) {
	require.Nil(t, mergeInstanceTags(nil, nil))
	require.Nil(t, mergeInstanceTags(&compute.Tags{}, nil))
}

// Helper function to create bool pointers
func boolPtr(b bool) *bool {
	return &b
}

func TestResolveAccessConfigsWithNetworkConfigDisabled(t *testing.T) {
	provider := &DefaultProvider{}
	nodeClass := &v1alpha1.GCENodeClass{
		Spec: v1alpha1.GCENodeClassSpec{
			NetworkConfig: &v1alpha1.NetworkConfig{
				EnableExternalIPAccess: boolPtr(false),
			},
		},
	}
	templateConfigs := []*compute.AccessConfig{
		{Type: "ONE_TO_ONE_NAT", Name: "External NAT"},
	}

	result := provider.resolveAccessConfigs(templateConfigs, nodeClass)

	require.Nil(t, result, "Should return nil when external IP is disabled")
}

func TestResolveAccessConfigsWithNetworkConfigEnabled(t *testing.T) {
	provider := &DefaultProvider{}
	nodeClass := &v1alpha1.GCENodeClass{
		Spec: v1alpha1.GCENodeClassSpec{
			NetworkConfig: &v1alpha1.NetworkConfig{
				EnableExternalIPAccess: boolPtr(true),
			},
		},
	}
	templateConfigs := []*compute.AccessConfig{
		{Type: "ONE_TO_ONE_NAT", Name: "External NAT"},
	}

	result := provider.resolveAccessConfigs(templateConfigs, nodeClass)

	require.Equal(t, templateConfigs, result)
}

func TestResolveAccessConfigsWithNoNetworkConfig(t *testing.T) {
	provider := &DefaultProvider{}
	nodeClass := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{}}
	templateConfigs := []*compute.AccessConfig{
		{Type: "ONE_TO_ONE_NAT", Name: "External NAT"},
	}

	result := provider.resolveAccessConfigs(templateConfigs, nodeClass)

	require.Equal(t, templateConfigs, result, "Should preserve template config when networkConfig is nil")
}

func TestResolveSubnetworkWithCustomSubnet(t *testing.T) {
	provider := &DefaultProvider{}
	customSubnet := "projects/my-project/regions/us-central1/subnetworks/custom-subnet"
	nodeClass := &v1alpha1.GCENodeClass{
		Spec: v1alpha1.GCENodeClassSpec{
			NetworkConfig: &v1alpha1.NetworkConfig{
				Subnetwork: customSubnet,
			},
		},
	}

	result := provider.resolveSubnetwork("projects/default/regions/us-central1/subnetworks/default", nodeClass)

	require.Equal(t, customSubnet, result)
}

func TestResolveSubnetworkWithNoNetworkConfig(t *testing.T) {
	provider := &DefaultProvider{}
	templateSubnet := "projects/default/regions/us-central1/subnetworks/default"
	nodeClass := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{}}

	result := provider.resolveSubnetwork(templateSubnet, nodeClass)

	require.Equal(t, templateSubnet, result, "Should use template subnet when networkConfig is nil")
}

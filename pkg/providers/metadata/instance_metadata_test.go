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

package metadata

import (
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
)

func TestInstanceMetadataCopiesOnlyExplicitSourceFieldsAndUserMetadata(t *testing.T) {
	source := &compute.Metadata{Items: []*compute.MetadataItems{
		{Key: KubeLabelsKey, Value: lo.ToPtr("old=value")},
		{Key: KubeEnvKey, Value: lo.ToPtr("KUBELET_ARGS: --node-labels=old=value")},
		{Key: "unowned-template-key", Value: lo.ToPtr("source")},
	}}

	instanceMetadata := NewInstanceMetadataFromSource(source)

	require.NoError(t, instanceMetadata.ReplaceBootstrapNodeLabels(map[string]string{"template": "label"}, map[string]string{"env": "label"}))
	instanceMetadata.ApplyUserMetadata(map[string]string{"unknown": "user", "empty": ""})
	require.NoError(t, instanceMetadata.SetProviderNodeLabels(map[string]string{"provider": "label"}))

	values := FromAPI(instanceMetadata.ToComputeMetadata())
	require.Equal(t, "user", values["unknown"])
	require.NotContains(t, values, "empty")
	require.NotContains(t, values, "unowned-template-key")
	require.Equal(t, "provider=label,template=label", values[KubeLabelsKey])
	require.Contains(t, values[KubeEnvKey], "--node-labels=env=label,provider=label")

	require.Equal(t, "old=value", FromAPI(source)[KubeLabelsKey])
}

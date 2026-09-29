/*
Copyright 2026 The CloudPilot AI Authors.

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
	"strings"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	kubeletconfig "k8s.io/kubelet/config/v1beta1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/metadata"
)

func TestValidateSourceKubeEnv(t *testing.T) {
	tests := []struct {
		name    string
		kubeEnv string
		want    string
	}{
		{
			name:    "complete",
			kubeEnv: requiredSourceKubeEnv + "KUBELET_ARGS: --v=2\n",
		},
		{
			name: "no kube-env",
			want: "missing required entries: CA_CERT, KUBE_MANIFESTS_TAR_URL, KUBERNETES_MASTER_NAME, SERVER_BINARY_TAR_HASH, SERVER_BINARY_TAR_URL",
		},
		{
			name:    "empty value",
			kubeEnv: strings.Replace(requiredSourceKubeEnv, "CA_CERT: test-ca", "CA_CERT: ", 1),
			want:    "missing required entries: CA_CERT",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := &compute.Metadata{}
			if tt.kubeEnv != "" {
				source.Items = []*compute.MetadataItems{{Key: metadata.KubeEnvKey, Value: lo.ToPtr(tt.kubeEnv)}}
			}
			target, err := metadata.FromSourceTemplate(source)
			require.NoError(t, err)

			err = validateSourceKubeEnv(target)
			if tt.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestApplyNodeClassKubeletConfigSetsImagePullParallelism(t *testing.T) {
	config := &kubeletconfig.KubeletConfiguration{}
	serialize := false
	maxParallel := int32(4)
	overlay := &v1alpha1.KubeletConfiguration{
		SerializeImagePulls:   &serialize,
		MaxParallelImagePulls: &maxParallel,
	}

	applyNodeClassKubeletConfig(config, overlay)

	require.NotNil(t, config.SerializeImagePulls)
	require.False(t, *config.SerializeImagePulls)
	require.NotNil(t, config.MaxParallelImagePulls)
	require.Equal(t, int32(4), *config.MaxParallelImagePulls)
}

func TestApplyNodeClassKubeletConfigLeavesImagePullParallelismUnsetWhenOverlayOmitsIt(t *testing.T) {
	config := &kubeletconfig.KubeletConfiguration{}
	maxPods := int32(64)
	overlay := &v1alpha1.KubeletConfiguration{MaxPods: &maxPods}

	applyNodeClassKubeletConfig(config, overlay)

	require.Nil(t, config.SerializeImagePulls)
	require.Nil(t, config.MaxParallelImagePulls)
	require.Equal(t, int32(64), config.MaxPods)
}

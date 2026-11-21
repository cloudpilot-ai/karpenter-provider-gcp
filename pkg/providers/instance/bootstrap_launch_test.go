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
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	"k8s.io/utils/ptr"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/metadata"
)

func TestNewLaunchSameArchitectureUsesOnePreparedImageAndCoreRelease(t *testing.T) {
	class := imagesReadyNodeClass()
	class.Spec.Disks = []v1alpha1.Disk{{Boot: true, SizeGiB: 100}}
	class.Status.Images[0].SourceImage = "projects/test/global/images/selected-pin"
	bundle := testLaunchImage(class).Bootstrap
	provider := makeProvider()
	provider.bootstrapProvider = bootstrapFunc(func(_ context.Context, version, arch string) (metadata.BootstrapRelease, error) {
		require.Equal(t, "1.36.4-gke.2046003", version)
		require.Equal(t, "amd64", arch, "matching source architecture still needs target release artifacts")
		return bundle, nil
	})
	selected, err := provider.resolveLaunchImage(context.Background(), class, makeNonGPUIT())
	require.NoError(t, err)

	// A later status selection must not change either half of this prepared launch.
	class.Status.Images[0].SourceImage = "projects/test/global/images/later-image"
	class.Status.Images[0].KubernetesVersion = "1.37.1-gke.123"
	const oldCore = "KUBELET_VERSION: v1.35.8-gke.1796000\n" +
		"SERVER_BINARY_TAR_URL: https://storage.googleapis.com/gke-release/kubernetes/release/v1.35.8-gke.1796000/kubernetes-server-linux-amd64.tar.gz\n" +
		"SERVER_BINARY_TAR_HASH: old-server-hash\nKUBE_MANIFESTS_TAR_URL: old-manifests\nKUBE_MANIFESTS_TAR_HASH: old-manifests-hash\n"
	source := makeSourceMetadata("max-pods-per-node=110")
	for _, item := range source.Items {
		if item.Key == metadata.KubeEnvKey {
			*item.Value += oldCore
		}
	}
	source.Items = append(source.Items,
		&compute.MetadataItems{Key: "configure-sh", Value: ptr.To("old configure")},
		&compute.MetadataItems{Key: "user-data", Value: ptr.To("old entrypoint")},
		&compute.MetadataItems{Key: "startup-script", Value: ptr.To("competing source entrypoint")},
	)
	instance, err := provider.buildInstance(context.Background(), spotOrOnDemandNodeClaim(), class, makeNonGPUIT(), source,
		makeCluster("projects/p/global/networks/default", "regions/us-central1/subnetworks/default", "pods", false),
		"us-central1-a", "prepared-launch", karpv1.CapacityTypeOnDemand, nil, 0, "pods", selected)
	require.NoError(t, err)
	require.Equal(t, "projects/test/global/images/selected-pin", instance.Disks[0].InitializeParams.SourceImage)
	env := kubeEnvFrom(t, instance)
	require.Contains(t, env, "KUBELET_VERSION: v1.36.4-gke.2046003")
	require.Contains(t, env, "SERVER_BINARY_TAR_URL: "+bundle.ServerBinaryURL)
	require.Contains(t, env, "SERVER_BINARY_TAR_HASH: "+bundle.ServerBinaryHash)
	require.Contains(t, env, "KUBE_MANIFESTS_TAR_URL: "+bundle.ManifestsURL)
	require.Contains(t, env, "KUBE_MANIFESTS_TAR_HASH: "+bundle.ManifestsHash)
	require.NotContains(t, env, "v1.35.8-gke.1796000")
	require.Equal(t, bundle.ConfigureScript, metadataValue(instance.Metadata, "configure-sh"))
	require.Equal(t, bundle.UserData, metadataValue(instance.Metadata, "user-data"))
	require.NotContains(t, metadataKeys(instance), "startup-script")
}

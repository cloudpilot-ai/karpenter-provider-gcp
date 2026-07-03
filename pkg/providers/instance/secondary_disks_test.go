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
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/metadata"
)

func TestSecondaryBootDisksFiltersOnlyConfiguredSecondaryDisks(t *testing.T) {
	nodeClass := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{Disks: []v1alpha1.Disk{
		{Boot: true, SecondaryBootImage: "global/images/boot-cache", SecondaryBootMode: "CONTAINER_IMAGE_CACHE"},
		{SecondaryBootImage: "", SecondaryBootMode: "CONTAINER_IMAGE_CACHE"},
		{SecondaryBootImage: "global/images/unspecified", SecondaryBootMode: "MODE_UNSPECIFIED"},
		{SecondaryBootImage: "projects/project-a/global/images/cache-image", SecondaryBootMode: "CONTAINER_IMAGE_CACHE"},
	}}}

	disks := secondaryBootDisks(nodeClass)

	require.Len(t, disks, 1)
	require.Equal(t, "projects/project-a/global/images/cache-image", disks[0].SecondaryBootImage)
}

func TestSecondaryBootDiskImageDeviceNameUsesGKEConvention(t *testing.T) {
	require.Equal(t, "gke-cache-image-disk", secondaryBootDiskImageDeviceName("projects/project-a/global/images/cache-image"))
}

func TestBuildInstance_DropsStaleSourceSecondaryBootDiskMetadata(t *testing.T) {
	provider := makeProvider()
	sourceMetadata := computeMetadataValues(map[string]string{
		metadata.KubeLabelsKey:    "max-pods-per-node=110,max-pods=110",
		metadata.KubeEnvKey:       "SECONDARY_BOOT_DISKS: /mnt/disks/gke-secondary-disks/stale-disk\nKUBELET_ARGS: --max-pods=110 --node-labels=max-pods-per-node=110,max-pods=110\n",
		metadata.KubeletConfigKey: "nodeStatusUpdateFrequency: 10s\n",
	})

	instance, err := provider.buildInstance(
		context.Background(),
		spotOrOnDemandNodeClaim(), &v1alpha1.GCENodeClass{}, makeNonGPUIT(), sourceMetadata,
		makeCluster("projects/p/global/networks/my-vpc", "regions/us-central1/subnetworks/my-subnet", "pods", false),
		"us-central1-a", "karpenter-secondary-boot-disk-test",
		karpv1.CapacityTypeOnDemand,
	)

	require.NoError(t, err)
	require.NotContains(t, kubeEnvFrom(t, instance), "SECONDARY_BOOT_DISKS")
	require.NotContains(t, kubeEnvFrom(t, instance), "stale-disk")
}

func TestBuildInstance_SecondaryBootDiskMetadata(t *testing.T) {
	provider := makeProvider()
	provider.projectID = "project-a"
	nodeClass := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{Disks: []v1alpha1.Disk{{
		SecondaryBootImage: "projects/project-a/global/images/cache-image",
		SecondaryBootMode:  "CONTAINER_IMAGE_CACHE",
	}}}}

	instance, err := provider.buildInstance(
		context.Background(),
		spotOrOnDemandNodeClaim(), nodeClass, makeNonGPUIT(), makeSourceMetadata("max-pods-per-node=110,max-pods=110"),
		makeCluster("projects/p/global/networks/my-vpc", "regions/us-central1/subnetworks/my-subnet", "pods", false),
		"us-central1-a", "karpenter-secondary-boot-disk-test",
		karpv1.CapacityTypeOnDemand,
	)

	require.NoError(t, err)
	require.Contains(t, kubeLabelsFrom(t, instance), "cloud.google.com.node-restriction.kubernetes.io/gke-secondary-boot-disk-cache-image=CONTAINER_IMAGE_CACHE.project-a")
	require.NotContains(t, kubeLabelsFrom(t, instance), "gke-secondary-boot-disk--cache-image")
	require.Contains(t, kubeEnvFrom(t, instance), "SECONDARY_BOOT_DISKS: /mnt/disks/gke-secondary-disks/gke-cache-image-disk")
}

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

	"github.com/go-openapi/swag"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

func TestNoPatchKubeEnv(t *testing.T) {
	meta := &compute.Metadata{
		Items: []*compute.MetadataItems{
			{},
		},
	}

	it := &cloudprovider.InstanceType{
		Name: "c4a-highmem-2",
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "arm64"),
			scheduling.NewRequirement(v1alpha1.LabelInstanceFamily, corev1.NodeSelectorOpIn, "c4a"),
		),
	}

	require.NoError(t, PatchKubeEnvForInstanceType(meta, it))

	got := swag.StringValue(meta.Items[0].Value)
	require.NotContains(t, got, "kubernetes-server-linux-arm64.tar.gz")
	require.NotContains(t, got, "kubernetes-server-linux-amd64.tar.gz")
	require.NotContains(t, got, "cloud.google.com/machine-family=c4a")
	require.NotContains(t, got, "arch=arm64")
	require.NotContains(t, got, "cloud.google.com/machine-family=e2")
}

func TestPatchKubeEnvForInstanceType_ARM64(t *testing.T) {
	meta := &compute.Metadata{
		Items: []*compute.MetadataItems{
			{
				Key: "kube-env",
				Value: swag.String(`SERVER_BINARY_TAR_URL:
  https://storage.googleapis.com/gke-release-eu/kubernetes/release/v1.34.3-gke.1444000/kubernetes-server-linux-amd64.tar.gz,
  https://storage.googleapis.com/gke-release/kubernetes/release/v1.34.3-gke.1444000/kubernetes-server-linux-amd64.tar.gz,
  https://storage.googleapis.com/gke-release-asia/kubernetes/release/v1.34.3-gke.1444000/kubernetes-server-linux-amd64.tar.gz
AUTOSCALER_ENV_VARS: cloud.google.com/machine-family=e2, arch=amd64; something=else
KUBELET_ARGS: cloud.google.com/machine-family=e2, arch=amd64; --v=2
`),
			},
		},
	}

	it := &cloudprovider.InstanceType{
		Name: "c4a-highmem-2",
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "arm64"),
			scheduling.NewRequirement(v1alpha1.LabelInstanceFamily, corev1.NodeSelectorOpIn, "c4a"),
		),
	}

	require.NoError(t, PatchKubeEnvForInstanceType(meta, it))

	got := swag.StringValue(meta.Items[0].Value)
	// SERVER_BINARY_TAR_URL is not patched; the ARM64 node pool template already has the correct URL+hash.
	require.Contains(t, got, "kubernetes-server-linux-amd64.tar.gz")
	require.Contains(t, got, "cloud.google.com/machine-family=c4a")
	require.Contains(t, got, "arch=arm64")
	require.NotContains(t, got, "cloud.google.com/machine-family=e2")
}

func TestPatchKubeEnvForInstanceType_AMD64(t *testing.T) {
	meta := &compute.Metadata{
		Items: []*compute.MetadataItems{
			{
				Key: "kube-env",
				Value: swag.String(`SERVER_BINARY_TAR_URL:
  https://storage.googleapis.com/gke-release/kubernetes/release/v1.34.3-gke.1444000/kubernetes-server-linux-arm64.tar.gz
AUTOSCALER_ENV_VARS: cloud.google.com/machine-family=c4a, arch=arm64;
KUBELET_ARGS: cloud.google.com/machine-family=c4a, arch=arm64;
`),
			},
		},
	}

	it := &cloudprovider.InstanceType{
		Name: "e2-standard-2",
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"),
			scheduling.NewRequirement(v1alpha1.LabelInstanceFamily, corev1.NodeSelectorOpIn, "e2"),
		),
	}

	require.NoError(t, PatchKubeEnvForInstanceType(meta, it))

	got := swag.StringValue(meta.Items[0].Value)
	// SERVER_BINARY_TAR_URL is not patched; the AMD64 node pool template already has the correct URL+hash.
	require.Contains(t, got, "kubernetes-server-linux-arm64.tar.gz")
	require.Contains(t, got, "cloud.google.com/machine-family=e2")
	require.Contains(t, got, "arch=amd64")
	require.NotContains(t, got, "cloud.google.com/machine-family=c4a")
}

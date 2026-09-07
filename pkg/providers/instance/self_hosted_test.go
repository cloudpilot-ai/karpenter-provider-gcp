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
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/metadata"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/operator/options"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
)

const selfHostedStartupScript = "#!/bin/bash\nset -euo pipefail\necho 'nodeup: bootstrap'\n"

func selfHostedContext() context.Context {
	return options.ToContext(context.Background(), &options.Options{
		ProvisionMode: options.ProvisionModeSelfHosted,
	})
}

func selfHostedNodeClass() *v1alpha1.GCENodeClass {
	return &v1alpha1.GCENodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "self-hosted"},
		Spec: v1alpha1.GCENodeClassSpec{
			StartupScript: ptr.To(selfHostedStartupScript),
			KubeletConfiguration: &v1alpha1.KubeletConfiguration{
				MaxPods: ptr.To(int32(110)),
			},
			Metadata: map[string]string{
				"kops-k8s-io-instance-group-name": "nodes-us-central1-a",
			},
			NetworkConfig: &v1alpha1.NetworkConfig{
				Subnetwork: "projects/test-project/regions/us-central1/subnetworks/nodes",
			},
			NetworkTags: []v1alpha1.NetworkTag{"poc0006-k8s-local-node"},
		},
	}
}

// selfHostedFakeProvider serves Subnetworks.Get from a fake compute endpoint and fails
// the test on any other API call — the self-hosted launch path must not touch anything
// else while building the instance.
func selfHostedFakeProvider(t *testing.T) *DefaultProvider {
	t.Helper()
	p := newFakeComputeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/projects/test-project/regions/us-central1/subnetworks/nodes") {
			writeJSON(w, &compute.Subnetwork{Network: "projects/test-project/global/networks/my-vpc"})
			return
		}
		t.Errorf("unexpected API call in self-hosted instance build: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	p.clusterName = "poc0006.k8s.local"
	p.clusterLocation = "us-central1"
	p.computeDefaultSA = "123-compute@developer.gserviceaccount.com"
	return p
}

func TestBuildInstance_SelfHostedRendersOnlyOwnedMetadataAndTags(t *testing.T) {
	t.Parallel()

	p := selfHostedFakeProvider(t)
	nodeClaim := onDemandNodeClaim()
	nodeClaim.Labels = map[string]string{karpv1.NodePoolLabelKey: "my-pool"}

	instance, err := p.buildInstance(selfHostedContext(), nodeClaim, selfHostedNodeClass(), amd64InstanceType(),
		nil, nil, "us-central1-a", "karpenter-test", karpv1.CapacityTypeOnDemand)
	require.NoError(t, err)

	// Metadata: exactly startup-script (verbatim), cluster-name (exact option value),
	// and spec.metadata — no GKE bootstrap keys.
	keys := make([]string, 0, len(instance.Metadata.Items))
	for _, item := range instance.Metadata.Items {
		keys = append(keys, item.Key)
	}
	require.ElementsMatch(t, []string{"cluster-name", "startup-script", "kops-k8s-io-instance-group-name"}, keys)
	require.Equal(t, selfHostedStartupScript, metadataValue(instance.Metadata, "startup-script"))
	require.Equal(t, "poc0006.k8s.local", metadataValue(instance.Metadata, "cluster-name"))
	require.Equal(t, "nodes-us-central1-a", metadataValue(instance.Metadata, "kops-k8s-io-instance-group-name"))

	// Tags: only NodeClass network tags, no gke-* firewall tags.
	require.Equal(t, []string{"poc0006-k8s-local-node"}, instance.Tags.Items)

	// Labels: karpenter identity + sanitized cluster GC labels, no goog-gke-*.
	for key := range instance.Labels {
		require.False(t, strings.HasPrefix(key, "goog-gke-"), "GKE-owned label %q must not be set in self-hosted mode", key)
	}
	require.Equal(t, "my-pool", instance.Labels[utils.SanitizeGCELabelValue(utils.LabelNodePoolKey)])
	require.Equal(t, "self-hosted", instance.Labels[utils.SanitizeGCELabelValue(utils.LabelGCENodeClassKey)])
	require.Equal(t, "poc0006-k8s-local", instance.Labels[utils.SanitizeGCELabelValue(utils.LabelClusterNameKey)])
	require.Equal(t, "us-central1", instance.Labels[utils.SanitizeGCELabelValue(utils.LabelClusterLocationKey)])

	// Network: derived from the subnetwork resource; private by default with no alias
	// ranges when subnetRangeName is unset.
	require.Len(t, instance.NetworkInterfaces, 1)
	iface := instance.NetworkInterfaces[0]
	require.Equal(t, "projects/test-project/global/networks/my-vpc", iface.Network)
	require.Equal(t, "projects/test-project/regions/us-central1/subnetworks/nodes", iface.Subnetwork)
	require.Empty(t, iface.AccessConfigs, "nodes must be private by default in self-hosted mode")
	require.Contains(t, iface.ForceSendFields, "AccessConfigs")
	require.Empty(t, iface.AliasIpRanges)
}

func TestBuildInstance_SelfHostedSpotKeepsSchedulingWithoutGKELabel(t *testing.T) {
	t.Parallel()

	p := selfHostedFakeProvider(t)
	nodeClaim := spotOrOnDemandNodeClaim()
	nodeClaim.Labels = map[string]string{karpv1.NodePoolLabelKey: "my-pool"}

	instance, err := p.buildInstance(selfHostedContext(), nodeClaim, selfHostedNodeClass(), amd64InstanceType(),
		nil, nil, "us-central1-a", "karpenter-test", karpv1.CapacityTypeSpot)
	require.NoError(t, err)

	require.Equal(t, "SPOT", instance.Scheduling.ProvisioningModel)
	require.True(t, instance.Scheduling.Preemptible)
	require.NotContains(t, instance.Labels, "goog-gke-node-pool-provisioning-model")
}

func TestBuildInstance_SelfHostedWithoutStartupScriptOmitsKey(t *testing.T) {
	t.Parallel()

	// startupScript is optional: images with baked-in bootstrap need none. The
	// startup-script key must be absent, while cluster-name still carries the exact
	// CLUSTER_NAME option value.
	p := selfHostedFakeProvider(t)
	nodeClass := selfHostedNodeClass()
	nodeClass.Spec.StartupScript = nil

	instance, err := p.buildInstance(selfHostedContext(), onDemandNodeClaim(), nodeClass, amd64InstanceType(),
		nil, nil, "us-central1-a", "karpenter-test", karpv1.CapacityTypeOnDemand)
	require.NoError(t, err)

	keys := make([]string, 0, len(instance.Metadata.Items))
	for _, item := range instance.Metadata.Items {
		keys = append(keys, item.Key)
	}
	require.NotContains(t, keys, "startup-script")
	require.Equal(t, "poc0006.k8s.local", metadataValue(instance.Metadata, "cluster-name"))
}

func TestBuildInstance_SelfHostedRejectsMetadataOverGCEByteLimit(t *testing.T) {
	t.Parallel()

	// Oversized payloads must fail before Instances.Insert with the offending key named.
	p := selfHostedFakeProvider(t)
	nodeClass := selfHostedNodeClass()
	nodeClass.Spec.StartupScript = ptr.To(strings.Repeat("x", metadata.MaxMetadataValueBytes+1))

	_, err := p.buildInstance(selfHostedContext(), onDemandNodeClaim(), nodeClass, amd64InstanceType(),
		nil, nil, "us-central1-a", "karpenter-test", karpv1.CapacityTypeOnDemand)

	require.ErrorContains(t, err, `metadata value for key "startup-script"`)
	require.ErrorContains(t, err, "per-value limit of 262144 bytes")
}

func TestSetupSelfHostedNetworkInterfaces_RequiresSubnetwork(t *testing.T) {
	t.Parallel()

	p := &DefaultProvider{}
	nodeClass := selfHostedNodeClass()
	nodeClass.Spec.NetworkConfig = nil

	_, err := p.setupSelfHostedNetworkInterfaces(context.Background(), nodeClass)

	require.ErrorContains(t, err, "spec.networkConfig.subnetwork is required in self-hosted mode")
}

func TestSetupSelfHostedNetworkInterfaces_ExplicitPublicAndAliasRange(t *testing.T) {
	t.Parallel()

	p := selfHostedFakeProvider(t)
	nodeClass := selfHostedNodeClass()
	nodeClass.Spec.NetworkConfig.EnablePrivateNodes = ptr.To(false)
	nodeClass.Spec.SubnetRangeName = ptr.To("pods")

	ifaces, err := p.setupSelfHostedNetworkInterfaces(context.Background(), nodeClass)
	require.NoError(t, err)

	require.Len(t, ifaces, 1)
	require.Equal(t, []*compute.AccessConfig{{Type: "ONE_TO_ONE_NAT", Name: "External NAT"}}, ifaces[0].AccessConfigs)
	require.Equal(t, []*compute.AliasIpRange{{IpCidrRange: "/24", SubnetworkRangeName: "pods"}}, ifaces[0].AliasIpRanges)
}

func TestSetupSelfHostedNetworkInterfaces_BareNameCanonicalized(t *testing.T) {
	t.Parallel()

	p := selfHostedFakeProvider(t)
	nodeClass := selfHostedNodeClass()
	nodeClass.Spec.NetworkConfig.Subnetwork = "nodes"
	nodeClass.Spec.NetworkConfig.AdditionalNetworkInterfaces = []v1alpha1.AdditionalNetworkInterface{
		{Network: "projects/test-project/global/networks/other-vpc", Subnetwork: "nodes"},
	}

	ifaces, err := p.setupSelfHostedNetworkInterfaces(context.Background(), nodeClass)
	require.NoError(t, err)
	require.Len(t, ifaces, 2)

	// A bare subnetwork name must be written to the NIC in canonical form — the raw
	// spec value is not a valid reference for Instances.Insert.
	require.Equal(t, "projects/test-project/global/networks/my-vpc", ifaces[0].Network)
	require.Equal(t, "projects/test-project/regions/us-central1/subnetworks/nodes", ifaces[0].Subnetwork)

	// An explicit network override is honored without a lookup, and the subnetwork is
	// still canonicalized.
	require.Equal(t, "projects/test-project/global/networks/other-vpc", ifaces[1].Network)
	require.Equal(t, "projects/test-project/regions/us-central1/subnetworks/nodes", ifaces[1].Subnetwork)
}

func TestParseSubnetworkRef(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name            string
		value           string
		expectedProject string
		expectedRegion  string
		expectedName    string
		expectErr       bool
	}{
		{
			name:            "bare name uses defaults",
			value:           "nodes",
			expectedProject: "default-project",
			expectedRegion:  "us-central1",
			expectedName:    "nodes",
		},
		{
			name:            "partial path",
			value:           "projects/other/regions/europe-west4/subnetworks/nodes",
			expectedProject: "other",
			expectedRegion:  "europe-west4",
			expectedName:    "nodes",
		},
		{
			name:            "full URL",
			value:           "https://www.googleapis.com/compute/v1/projects/other/regions/europe-west4/subnetworks/nodes",
			expectedProject: "other",
			expectedRegion:  "europe-west4",
			expectedName:    "nodes",
		},
		{
			name:            "region-relative path uses default project",
			value:           "regions/europe-west4/subnetworks/nodes",
			expectedProject: "default-project",
			expectedRegion:  "europe-west4",
			expectedName:    "nodes",
		},
		{
			name:      "path without subnetworks segment",
			value:     "projects/other/global/networks/my-vpc",
			expectErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			project, region, name, err := parseSubnetworkRef(tc.value, "default-project", "us-central1")
			if tc.expectErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.expectedProject, project)
			require.Equal(t, tc.expectedRegion, region)
			require.Equal(t, tc.expectedName, name)
		})
	}
}

// TestBuildInstance_GKEModeUnchangedWithoutOptions proves the gke path still renders
// bootstrap metadata and GKE tags/labels when no provision mode is set (options absent
// from ctx == gke mode), guarding against mode-gate decay.
func TestBuildInstance_GKEModeUnchangedWithoutOptions(t *testing.T) {
	t.Parallel()

	p := makeProvider()
	nodeClaim := onDemandNodeClaim()
	nodeClaim.Labels = map[string]string{karpv1.NodePoolLabelKey: "my-pool"}
	nodeClass := &v1alpha1.GCENodeClass{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	instanceType := &cloudprovider.InstanceType{
		Name: "e2-standard-2",
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"),
		),
		Overhead: &cloudprovider.InstanceTypeOverhead{KubeReserved: corev1.ResourceList{}},
	}

	instance, err := p.buildInstance(context.Background(), nodeClaim, nodeClass, instanceType,
		makeSourceMetadata("max-pods-per-node=110"),
		makeCluster("projects/p/global/networks/my-vpc", "regions/us-central1/subnetworks/my-subnet", "pods", false),
		"us-central1-a", "karpenter-test", karpv1.CapacityTypeOnDemand)
	require.NoError(t, err)

	require.NotEmpty(t, kubeLabelsFrom(t, instance))
	require.NotEmpty(t, kubeEnvFrom(t, instance))
	require.Contains(t, instance.Tags.Items, "gke-"+p.clusterName+"-node")
	require.Contains(t, instance.Labels, "goog-gke-node")
	require.Equal(t, "on-demand", instance.Labels["goog-gke-node-pool-provisioning-model"])
}

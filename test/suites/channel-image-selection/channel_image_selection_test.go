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

package channelimageselection_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

// familyChannelTestCase describes a channel/version-based image selection scenario.
type familyChannelTestCase struct {
	family  string
	channel string // set for channel tests
	version string // set for version tests
}

var _ = DescribeTable("ChannelImageSelection",
	func(ctx SpecContext, tc familyChannelTestCase) {
		runChannelImageSelectionTest(ctx, tc)
	},
	// family: ContainerOptimizedOS, channel: stable — resolves GKE build via server config
	Entry("COS / channel: stable / amd64 / on-demand",
		familyChannelTestCase{family: gcpv1alpha1.ImageFamilyContainerOptimizedOS, channel: gcpv1alpha1.ImageChannelStable},
		SpecTimeout(15*time.Minute)),
	// family: ContainerOptimizedOS, version: latest — equivalent to alias ContainerOptimizedOS@latest
	Entry("COS / version: latest / amd64 / on-demand",
		familyChannelTestCase{family: gcpv1alpha1.ImageFamilyContainerOptimizedOS, version: "latest"},
		SpecTimeout(15*time.Minute)),
	// family: Ubuntu2404, version: latest
	Entry("Ubuntu2404 / version: latest / amd64 / on-demand",
		familyChannelTestCase{family: gcpv1alpha1.ImageFamilyUbuntu2404, version: "latest"},
		SpecTimeout(15*time.Minute)),
)

func runChannelImageSelectionTest(ctx context.Context, tc familyChannelTestCase) {
	prefix := environment.TestPrefix(karpv1.ArchitectureAmd64, karpv1.CapacityTypeOnDemand, "channel-img")
	suffix := environment.UniqueSuffix()
	name := prefix + "-" + suffix

	GinkgoWriter.Printf("[setup] family=%s channel=%s version=%s nodePool=%s\n",
		tc.family, tc.channel, tc.version, name)

	var provisionedNodeName string
	DeferCleanup(func(ctx context.Context) {
		env.DeleteDeployment(ctx, name)
		env.DeleteNodePool(ctx, name)
		env.DeleteNodeClass(ctx, name)
		if provisionedNodeName != "" {
			Expect(env.WaitForNodeRemoval(ctx, provisionedNodeName)).To(Succeed())
		}
	})

	if tc.channel != "" {
		env.CreateNodeClassWithFamilyChannel(ctx, name, tc.family, tc.channel)
	} else {
		env.CreateNodeClassWithFamilyVersion(ctx, name, tc.family, tc.version)
	}
	env.WaitForNodeClassReady(ctx, name)

	// Verify the resolved image version is consistent with the requested selector.
	sourceImages := env.NodeClassSourceImages(ctx, name)
	Expect(sourceImages).NotTo(BeEmpty(),
		"no images in status.images after NodeClass became Ready (family=%s channel=%s version=%s)",
		tc.family, tc.channel, tc.version)

	switch {
	case tc.version == "latest" && tc.family == gcpv1alpha1.ImageFamilyContainerOptimizedOS:
		// For version: latest, the resolved image must match the independently-queried latest COS image.
		expected := env.ResolveCurrentCOSImage(ctx)
		Expect(sourceImages[0]).To(Equal(expected),
			"resolved COS image does not match independently-queried latest image")
	case tc.version == "latest" && tc.family == gcpv1alpha1.ImageFamilyUbuntu2404:
		// For version: latest, the resolved image must embed the independently-queried Ubuntu version.
		expectedVer := env.ResolveCurrentUbuntuVersion(ctx)
		Expect(sourceImages[0]).To(ContainSubstring(expectedVer),
			"resolved Ubuntu2404 image does not contain expected version %s", expectedVer)
	case tc.channel != "":
		// For channel-based selection, verify the image is from the correct GCP project.
		switch tc.family {
		case gcpv1alpha1.ImageFamilyContainerOptimizedOS:
			Expect(sourceImages[0]).To(ContainSubstring("gke-node-images"),
				"expected COS image from gke-node-images project, got: %s", sourceImages[0])
		case gcpv1alpha1.ImageFamilyUbuntu2404:
			Expect(sourceImages[0]).To(ContainSubstring("ubuntu-os-gke-cloud"),
				"expected Ubuntu image from ubuntu-os-gke-cloud project, got: %s", sourceImages[0])
		}
	}

	env.CreateNodePool(ctx, name, name, environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2"},
		InstanceTypes: []string{"n2-standard-2", "n2-standard-4"},
	})
	env.WaitForNodePoolReady(ctx, name)
	env.CreateDeployment(ctx, name, name, name, karpv1.ArchitectureAmd64)

	env.WaitForNodeClaimLaunched(ctx, name)
	pod := env.WaitForRunningPod(ctx, name)
	Expect(pod.Spec.NodeName).NotTo(BeEmpty())

	node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	provisionedNodeName = node.Name

	Expect(environment.IsNodeReady(node)).To(BeTrue(), "node %s is not Ready", node.Name)
	Expect(node.Labels[karpv1.NodeRegisteredLabelKey]).To(Equal("true"))
	Expect(node.Labels[karpv1.NodePoolLabelKey]).To(Equal(name))
	Expect(node.Labels[karpv1.CapacityTypeLabelKey]).To(Equal(karpv1.CapacityTypeOnDemand))
}

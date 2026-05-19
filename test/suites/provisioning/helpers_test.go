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

package provisioning_test

import (
	"context"
	"regexp"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

var cosVersionFromImageRe = regexp.MustCompile(`cos-(\d+-\d+-\d+-\d+)-c-pre`)

func osSlug(imageFamily string) string {
	if imageFamily == gcpv1alpha1.ImageFamilyUbuntu {
		return "ubuntu"
	}
	return "cos"
}

func runProvisioningTest(ctx context.Context, tc environment.TestCase) {
	prefix := environment.TestPrefix(tc.Arch, tc.CapacityType, osSlug(tc.ImageFamily), "provisioning")
	suffix := environment.UniqueSuffix()
	name := prefix + "-" + suffix

	GinkgoWriter.Printf("[setup] arch=%s capacityType=%s os=%s nodePool=%s\n",
		tc.Arch, tc.CapacityType, tc.ImageFamily, name)

	initialNodes := env.AllNodeNames(ctx)

	var provisionedNodeName string
	DeferCleanup(func(ctx context.Context) {
		env.DeleteDeployment(ctx, name)
		env.DeleteNodePool(ctx, name)
		env.DeleteNodeClass(ctx, name)
		if provisionedNodeName != "" {
			Expect(env.WaitForNodeRemoval(ctx, provisionedNodeName)).To(Succeed())
		}
	})

	imageFamily := tc.ImageFamily
	if imageFamily == "" {
		imageFamily = gcpv1alpha1.ImageFamilyContainerOptimizedOS
	}
	env.CreateNodeClass(ctx, name, imageFamily)
	env.WaitForNodeClassReady(ctx, name)
	env.CreateNodePool(ctx, name, name, tc)
	env.WaitForNodePoolReady(ctx, name)
	env.CreateDeployment(ctx, name, name, name, tc.Arch)

	env.WaitForNodeClaimLaunched(ctx, name)
	pod := env.WaitForRunningPod(ctx, name)
	Expect(pod.Spec.NodeName).NotTo(BeEmpty())

	node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	provisionedNodeName = node.Name

	_, existedBefore := initialNodes[node.Name]
	Expect(existedBefore).To(BeFalse(), "expected a newly provisioned node, got a pre-existing one")
	Expect(environment.IsNodeReady(node)).To(BeTrue(), "node %s is not Ready", node.Name)
	Expect(node.Labels[karpv1.NodeRegisteredLabelKey]).To(Equal("true"))
	Expect(node.Labels[karpv1.NodePoolLabelKey]).To(Equal(name))
	Expect(node.Labels[karpv1.CapacityTypeLabelKey]).To(Equal(tc.CapacityType))
	Expect(node.Labels[corev1.LabelArchStable]).To(Equal(tc.Arch))
	Expect(tc.Families).To(ContainElement(node.Labels[gcpv1alpha1.LabelInstanceFamily]))
	Expect(tc.InstanceTypes).To(ContainElement(node.Labels[corev1.LabelInstanceTypeStable]))

	env.WaitForKubeProxyRunning(ctx, provisionedNodeName)
}

// runPinnedAliasTest provisions a node using a GCENodeClass with the given
// alias and verifies the resolved source image contains expectedImageSubstring.
func runPinnedAliasTest(ctx context.Context, alias, expectedImageSubstring string) {
	prefix := "amd64-od-pinned"
	suffix := environment.UniqueSuffix()
	name := prefix + "-" + suffix

	GinkgoWriter.Printf("[setup] alias=%s nodePool=%s\n", alias, name)

	initialNodes := env.AllNodeNames(ctx)

	var provisionedNodeName string
	DeferCleanup(func(ctx context.Context) {
		env.DeleteDeployment(ctx, name)
		env.DeleteNodePool(ctx, name)
		env.DeleteNodeClass(ctx, name)
		if provisionedNodeName != "" {
			Expect(env.WaitForNodeRemoval(ctx, provisionedNodeName)).To(Succeed())
		}
	})

	env.CreateNodeClassWithAlias(ctx, name, alias)
	env.WaitForNodeClassReady(ctx, name)

	images := env.NodeClassSourceImages(ctx, name)
	Expect(images).NotTo(BeEmpty(), "GCENodeClass %s has no status.images after becoming Ready", name)
	Expect(images[0]).To(ContainSubstring(expectedImageSubstring),
		"expected source image to contain %q, got %q", expectedImageSubstring, images[0])

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

	_, existedBefore := initialNodes[node.Name]
	Expect(existedBefore).To(BeFalse(), "expected a newly provisioned node, got a pre-existing one")
	Expect(environment.IsNodeReady(node)).To(BeTrue(), "node %s is not Ready", node.Name)

	env.WaitForKubeProxyRunning(ctx, provisionedNodeName)
}

// runImageIDTest provisions a node using a GCENodeClass that selects an image
// by exact resource URL (id selector) and verifies the resolved source image
// matches the provided imageID exactly.
func runImageIDTest(ctx context.Context, imageFamily, imageID string) {
	prefix := "amd64-od-imageid"
	suffix := environment.UniqueSuffix()
	name := prefix + "-" + suffix

	GinkgoWriter.Printf("[setup] imageFamily=%s imageID=%s nodePool=%s\n", imageFamily, imageID, name)

	initialNodes := env.AllNodeNames(ctx)

	var provisionedNodeName string
	DeferCleanup(func(ctx context.Context) {
		env.DeleteDeployment(ctx, name)
		env.DeleteNodePool(ctx, name)
		env.DeleteNodeClass(ctx, name)
		if provisionedNodeName != "" {
			Expect(env.WaitForNodeRemoval(ctx, provisionedNodeName)).To(Succeed())
		}
	})

	env.CreateNodeClassWithImageID(ctx, name, imageFamily, imageID)
	env.WaitForNodeClassReady(ctx, name)

	images := env.NodeClassSourceImages(ctx, name)
	Expect(images).NotTo(BeEmpty(), "GCENodeClass %s has no status.images after becoming Ready", name)
	Expect(images[0]).To(Equal(imageID),
		"expected source image to equal %q, got %q", imageID, images[0])

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

	_, existedBefore := initialNodes[node.Name]
	Expect(existedBefore).To(BeFalse(), "expected a newly provisioned node, got a pre-existing one")
	Expect(environment.IsNodeReady(node)).To(BeTrue(), "node %s is not Ready", node.Name)

	env.WaitForKubeProxyRunning(ctx, provisionedNodeName)
}

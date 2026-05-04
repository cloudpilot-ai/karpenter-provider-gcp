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

package drift_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

var _ = Describe("Drift", func() {
	// Requirement drift: provision a node, then update the NodePool to exclude the
	// running instance type. Karpenter detects requirement drift and replaces
	// the node with one of the remaining allowed types.
	It("should replace a drifted amd64 on-demand node", func(ctx SpecContext) {
		runDriftTest(ctx, environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2"},
			InstanceTypes: []string{"n2-standard-2", "n2-standard-4"},
		})
	}, SpecTimeout(25*time.Minute))

	// NodeClass drift: provision a node, then mutate the GCENodeClass spec.
	// The hash controller updates the NodeClass hash annotation, the disruption
	// controller marks the NodeClaim as drifted, and Karpenter replaces the node.
	It("should replace a node when GCENodeClass metadata changes", func(ctx SpecContext) {
		runNodeClassDriftTest(ctx, environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2"},
			InstanceTypes: []string{"n2-standard-2"},
		})
	}, SpecTimeout(30*time.Minute))
})

func runNodeClassDriftTest(ctx context.Context, tc environment.TestCase) {
	prefix := environment.TestPrefix(tc.Arch, tc.CapacityType, "ncclass-drift")
	suffix := environment.UniqueSuffix()
	name := prefix + "-" + suffix

	GinkgoWriter.Printf("[setup] nodeclass-drift arch=%s capacityType=%s nodePool=%s\n",
		tc.Arch, tc.CapacityType, name)

	var firstNodeName string
	DeferCleanup(func(ctx context.Context) {
		env.DeleteDeployment(ctx, name)
		env.DeleteNodePool(ctx, name)
		env.DeleteNodeClass(ctx, name)
		if firstNodeName != "" {
			_ = env.WaitForNodeRemoval(ctx, firstNodeName)
		}
	})

	env.CreateNodeClass(ctx, name, gcpv1alpha1.ImageFamilyContainerOptimizedOS)
	env.WaitForNodeClassReady(ctx, name)
	env.CreateNodePool(ctx, name, name, tc)
	env.WaitForNodePoolReady(ctx, name)
	env.CreateDeployment(ctx, name, name, name, tc.Arch)
	env.WaitForNodeClaimLaunched(ctx, name)
	firstPod := env.WaitForRunningPod(ctx, name)
	Expect(firstPod.Spec.NodeName).NotTo(BeEmpty())
	firstNodeName = firstPod.Spec.NodeName

	GinkgoWriter.Printf("[drift] first node: %s; adding spec.metadata entry to trigger NodeClassDrift\n", firstNodeName)

	// Mutate spec.metadata — a no-op for the running node but changes the hash.
	env.AddNodeClassMetadataEntry(ctx, name, "drift-test", "triggered")

	GinkgoWriter.Printf("[drift] GCENodeClass patched; waiting for replacement node...\n")

	replacementPod := env.WaitForPodOnDifferentNode(ctx, name, firstNodeName, environment.ReplacementTimeout)
	Expect(replacementPod.Spec.NodeName).NotTo(Equal(firstNodeName))

	replacementNode, err := env.KubeClient.CoreV1().Nodes().Get(ctx, replacementPod.Spec.NodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	replacementType := replacementNode.Labels[corev1.LabelInstanceTypeStable]
	Expect(tc.InstanceTypes).To(ContainElement(replacementType),
		"replacement node %s has unexpected instance type %s", replacementNode.Name, replacementType)

	GinkgoWriter.Printf("[drift] replacement node: %s instanceType=%s\n", replacementNode.Name, replacementType)

	Expect(env.WaitForNodeRemoval(ctx, firstNodeName)).To(Succeed())
	firstNodeName = ""
}

func runDriftTest(ctx context.Context, tc environment.TestCase) {
	prefix := environment.TestPrefix(tc.Arch, tc.CapacityType, "drift")
	suffix := environment.UniqueSuffix()
	name := prefix + "-" + suffix

	GinkgoWriter.Printf("[setup] drift arch=%s capacityType=%s nodePool=%s\n",
		tc.Arch, tc.CapacityType, name)

	var firstNodeName string
	DeferCleanup(func(ctx context.Context) {
		env.DeleteDeployment(ctx, name)
		env.DeleteNodePool(ctx, name)
		env.DeleteNodeClass(ctx, name)
		if firstNodeName != "" {
			_ = env.WaitForNodeRemoval(ctx, firstNodeName)
		}
	})

	env.CreateNodeClass(ctx, name, gcpv1alpha1.ImageFamilyContainerOptimizedOS)
	env.WaitForNodeClassReady(ctx, name)
	env.CreateNodePool(ctx, name, name, tc)
	env.WaitForNodePoolReady(ctx, name)
	env.CreateDeployment(ctx, name, name, name, tc.Arch)
	env.WaitForNodeClaimLaunched(ctx, name)
	firstPod := env.WaitForRunningPod(ctx, name)
	Expect(firstPod.Spec.NodeName).NotTo(BeEmpty())
	firstNodeName = firstPod.Spec.NodeName

	// Record which instance type was provisioned.
	firstNode, err := env.KubeClient.CoreV1().Nodes().Get(ctx, firstNodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	provisionedType := firstNode.Labels[corev1.LabelInstanceTypeStable]
	Expect(provisionedType).NotTo(BeEmpty(), "node %s has no instance-type label", firstNodeName)

	GinkgoWriter.Printf("[drift] first node: %s instanceType=%s; updating NodePool to exclude it\n",
		firstNodeName, provisionedType)

	// Exclude the provisioned instance type — the node is now drifted.
	remaining := make([]string, 0, len(tc.InstanceTypes)-1)
	for _, t := range tc.InstanceTypes {
		if t != provisionedType {
			remaining = append(remaining, t)
		}
	}
	Expect(remaining).NotTo(BeEmpty(), "no remaining instance types after excluding %s", provisionedType)
	env.UpdateNodePoolInstanceTypes(ctx, name, remaining)

	GinkgoWriter.Printf("[drift] NodePool updated; waiting for replacement on one of %v...\n", remaining)

	replacementPod := env.WaitForPodOnDifferentNode(ctx, name, firstNodeName, environment.ReplacementTimeout)
	Expect(replacementPod.Spec.NodeName).NotTo(Equal(firstNodeName))

	// Replacement node must use one of the remaining (non-drifted) instance types.
	replacementNode, err := env.KubeClient.CoreV1().Nodes().Get(ctx, replacementPod.Spec.NodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	replacementType := replacementNode.Labels[corev1.LabelInstanceTypeStable]
	Expect(remaining).To(ContainElement(replacementType),
		"replacement node %s has instance type %s not in remaining set %v",
		replacementNode.Name, replacementType, remaining)

	GinkgoWriter.Printf("[drift] replacement node: %s instanceType=%s\n", replacementNode.Name, replacementType)

	Expect(env.WaitForNodeRemoval(ctx, firstNodeName)).To(Succeed())
	firstNodeName = ""
}

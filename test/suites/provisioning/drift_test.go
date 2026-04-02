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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

var _ = Describe("Drift", func() {
	// Drift test: provision a node, then update the NodePool to exclude the
	// running instance type. Karpenter detects requirement drift and replaces
	// the node with one of the remaining allowed types.
	It("should replace a drifted amd64 on-demand node", func(ctx SpecContext) {
		runDriftTest(ctx, provisioningCase{
			capacityType: karpv1.CapacityTypeOnDemand,
			arch:         karpv1.ArchitectureAmd64,
			// Three types so after excluding the provisioned one at least two remain.
			families:      []string{"e2", "n2"},
			instanceTypes: []string{"e2-medium", "e2-standard-2", "n2-standard-2"},
		})
	}, SpecTimeout(25*time.Minute))
})

func runDriftTest(ctx context.Context, tc provisioningCase) {
	prefix := testPrefix(tc.arch, tc.capacityType) + "-drift"
	suffix := uniqueSuffix()
	nodeClassName := prefix + "-nc-" + suffix
	nodePoolName := prefix + "-np-" + suffix
	deployName := prefix + "-dep-" + suffix
	appLabel := prefix + "-" + suffix

	GinkgoWriter.Printf("[setup] drift arch=%s capacityType=%s nodePool=%s\n",
		tc.arch, tc.capacityType, nodePoolName)

	var firstNodeName string
	DeferCleanup(func(ctx context.Context) {
		deleteDeployment(ctx, deployName)
		deleteNodePool(ctx, nodePoolName)
		deleteNodeClass(ctx, nodeClassName)
		if firstNodeName != "" {
			_ = env.WaitForNodeRemoval(ctx, firstNodeName)
		}
	})

	createNodeClass(ctx, nodeClassName)
	createNodePool(ctx, nodePoolName, nodeClassName, tc)
	createDeployment(ctx, deployName, appLabel, nodePoolName, tc.arch)

	firstPod := waitForRunningPod(ctx, appLabel)
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
	remaining := make([]string, 0, len(tc.instanceTypes)-1)
	for _, t := range tc.instanceTypes {
		if t != provisionedType {
			remaining = append(remaining, t)
		}
	}
	Expect(remaining).NotTo(BeEmpty(), "no remaining instance types after excluding %s", provisionedType)
	updateNodePoolInstanceTypes(ctx, nodePoolName, remaining)

	GinkgoWriter.Printf("[drift] NodePool updated; waiting for replacement on one of %v...\n", remaining)

	replacementTimeout := 2 * environment.ProvisioningTimeout
	replacementPod := waitForPodOnDifferentNode(ctx, appLabel, firstNodeName, replacementTimeout)
	Expect(replacementPod.Spec.NodeName).NotTo(Equal(firstNodeName))

	// Replacement node must use one of the remaining (non-drifted) instance types.
	replacementNode, err := env.KubeClient.CoreV1().Nodes().Get(ctx, replacementPod.Spec.NodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	replacementType := replacementNode.Labels[corev1.LabelInstanceTypeStable]
	Expect(remaining).To(ContainElement(replacementType),
		"replacement node %s has instance type %s not in remaining set %v",
		replacementNode.Name, replacementType, remaining)

	GinkgoWriter.Printf("[drift] replacement node: %s instanceType=%s\n", replacementNode.Name, replacementType)

	// Original node must be gone.
	Expect(env.WaitForNodeRemoval(ctx, firstNodeName)).To(Succeed())
	firstNodeName = ""
}

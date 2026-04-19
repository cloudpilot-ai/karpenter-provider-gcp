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

package repair_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

var _ = Describe("NodeRepair", func() {
	// This test verifies that Karpenter replaces a node whose KernelDeadlock
	// condition has been True beyond the configured toleration duration.
	//
	// The KernelDeadlock condition is patched directly onto the node status via
	// the Kubernetes API — no actual kernel failure is induced. The karpenter
	// node.health controller monitors this condition (True = problem polarity,
	// as set by GKE Node Problem Detector) and deletes the node after the
	// toleration window expires, allowing a replacement to be provisioned.
	It("should replace a node whose KernelDeadlock condition has been True beyond the toleration",
		func(ctx SpecContext) {
			runRepairTest(ctx, environment.TestCase{
				CapacityType: karpv1.CapacityTypeOnDemand,
				Arch:         karpv1.ArchitectureAmd64,
				Families:     []string{"n2"},
			})
		}, SpecTimeout(environment.NodeRepairTimeout+environment.ProvisioningTimeout))
})

func runRepairTest(ctx context.Context, tc environment.TestCase) {
	prefix := environment.TestPrefix(tc.Arch, tc.CapacityType, "repair")
	suffix := environment.UniqueSuffix()
	name := prefix + "-" + suffix

	GinkgoWriter.Printf("[setup] repair arch=%s capacityType=%s nodePool=%s\n",
		tc.Arch, tc.CapacityType, name)

	var originalNodeName string
	DeferCleanup(func(ctx context.Context) {
		env.DeleteDeployment(ctx, name)
		env.DeleteNodePool(ctx, name)
		env.DeleteNodeClass(ctx, name)
		if originalNodeName != "" {
			_ = env.WaitForNodeRemoval(ctx, originalNodeName)
		}
	})

	env.CreateNodeClass(ctx, name)
	env.CreateNodePool(ctx, name, name, tc)
	env.CreateDeployment(ctx, name, name, name, tc.Arch)

	firstPod := env.WaitForRunningPod(ctx, name)
	Expect(firstPod.Spec.NodeName).NotTo(BeEmpty())
	originalNodeName = firstPod.Spec.NodeName

	GinkgoWriter.Printf("[repair] pod running on node %s; patching KernelDeadlock=True\n", originalNodeName)

	// Simulate a kernel deadlock by setting the GKE NPD condition directly.
	// KernelDeadlock toleration is 5 minutes — the node.health controller will
	// delete the node and karpenter will provision a replacement.
	env.PatchNodeCondition(ctx, originalNodeName,
		"KernelDeadlock", corev1.ConditionTrue,
		"TestSimulation", "e2e repair test: simulated kernel deadlock")

	GinkgoWriter.Printf("[repair] KernelDeadlock patched; waiting up to %v for pod on new node...\n",
		environment.NodeRepairTimeout)

	replacementPod := env.WaitForPodOnDifferentNode(ctx, name, originalNodeName, environment.NodeRepairTimeout)
	Expect(replacementPod.Spec.NodeName).NotTo(Equal(originalNodeName),
		"pod must move to a replacement node after KernelDeadlock repair")

	GinkgoWriter.Printf("[repair] pod rescheduled to replacement node %s; verifying original node removed\n",
		replacementPod.Spec.NodeName)

	Expect(env.WaitForNodeRemoval(ctx, originalNodeName)).To(Succeed(),
		"original node %s must be deleted after KernelDeadlock repair", originalNodeName)

	GinkgoWriter.Printf("[repair] node %s removed; repair verified\n", originalNodeName)

	// Clear the tracking variable so DeferCleanup does not wait again.
	originalNodeName = ""

	// Allow a small settling window so karpenter marks the replacement healthy.
	Consistently(func(g Gomega) {
		pod := env.WaitForRunningPod(ctx, name)
		g.Expect(pod.Spec.NodeName).To(Equal(replacementPod.Spec.NodeName))
	}, 30*time.Second, 10*time.Second).Should(Succeed(),
		"replacement pod must stay Running on the new node")
}

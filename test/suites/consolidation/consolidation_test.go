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

package consolidation_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

var _ = Describe("Consolidation", func() {
	// Exercises the disruption controller's WhenEmptyOrUnderutilized path:
	// provision a node, empty it by scaling to 0, and verify karpenter removes
	// the node without the test deleting the NodePool first.
	It("should consolidate an empty amd64 on-demand node", func(ctx SpecContext) {
		runConsolidationTest(ctx, environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2"},
			InstanceTypes: []string{"n2-standard-2", "n2-standard-4"},
		})
	}, SpecTimeout(15*time.Minute))
})

func runConsolidationTest(ctx context.Context, tc environment.TestCase) {
	prefix := environment.TestPrefix(tc.Arch, tc.CapacityType, "consolidation")
	suffix := environment.UniqueSuffix()
	name := prefix + "-" + suffix

	GinkgoWriter.Printf("[setup] consolidation arch=%s capacityType=%s nodePool=%s\n",
		tc.Arch, tc.CapacityType, name)

	var provisionedNodeName string
	DeferCleanup(func(ctx context.Context) {
		env.DeleteDeployment(ctx, name)
		env.DeleteNodePool(ctx, name)
		env.DeleteNodeClass(ctx, name)
		if provisionedNodeName != "" {
			// Node may already be gone if consolidation succeeded; ignore the error.
			_ = env.WaitForNodeRemoval(ctx, provisionedNodeName)
		}
	})

	env.CreateNodeClass(ctx, name)
	env.CreateNodePool(ctx, name, name, tc)
	env.CreateDeployment(ctx, name, name, name, tc.Arch)

	pod := env.WaitForRunningPod(ctx, name)
	Expect(pod.Spec.NodeName).NotTo(BeEmpty())
	provisionedNodeName = pod.Spec.NodeName

	GinkgoWriter.Printf("[consolidation] node provisioned: %s; scaling deployment to 0\n", provisionedNodeName)

	// Empty the node — karpenter should consolidate it within consolidateAfter + VM deletion time.
	env.ScaleDeployment(ctx, name, 0)

	GinkgoWriter.Printf("[consolidation] waiting for node %s to be removed...\n", provisionedNodeName)
	Expect(env.WaitForNodeRemoval(ctx, provisionedNodeName)).To(Succeed())
	GinkgoWriter.Printf("[consolidation] node %s removed by consolidation\n", provisionedNodeName)

	// Mark as gone so DeferCleanup skips the redundant WaitForNodeRemoval.
	provisionedNodeName = ""

	// NodePool and NodeClass must still exist — consolidation removed the node,
	// it did not delete the pool.
	claims, err := env.ListNodeClaims(ctx)
	Expect(err).NotTo(HaveOccurred())
	for _, c := range claims {
		Expect(c.GetLabels()[karpv1.NodePoolLabelKey]).NotTo(Equal(name),
			"unexpected NodeClaim for nodePool %s after consolidation", name)
	}
}

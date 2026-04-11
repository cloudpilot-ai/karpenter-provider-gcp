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

package expiration_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

var _ = Describe("Expiration", func() {
	// Expiration test: set expireAfter on the NodePool so nodes are expired as
	// soon as they register (1m < typical provisioning time ~5-7m), then verify
	// that karpenter replaces the node with a fresh one and the pod migrates.
	It("should replace an expired amd64 on-demand node", func(ctx SpecContext) {
		runExpirationTest(ctx, environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2"},
			InstanceTypes: []string{"n2-standard-2", "n2-standard-4"},
		})
	}, SpecTimeout(30*time.Minute))
})

func runExpirationTest(ctx context.Context, tc environment.TestCase) {
	prefix := environment.TestPrefix(tc.Arch, tc.CapacityType, "expiration")
	suffix := environment.UniqueSuffix()
	name := prefix + "-" + suffix

	// expireAfter must be longer than GCP provisioning time (~1.5m) so the first
	// node has time to register and the pod to run before the expiry fires.
	// At T+3m karpenter replaces the node; the replacement is ready at ~T+5m.
	const expireAfter = "3m"

	GinkgoWriter.Printf("[setup] expiration arch=%s capacityType=%s nodePool=%s expireAfter=%s\n",
		tc.Arch, tc.CapacityType, name, expireAfter)

	var firstNodeName string
	DeferCleanup(func(ctx context.Context) {
		env.DeleteDeployment(ctx, name)
		env.DeleteNodePool(ctx, name)
		env.DeleteNodeClass(ctx, name)
		if firstNodeName != "" {
			_ = env.WaitForNodeRemoval(ctx, firstNodeName)
		}
	})

	env.CreateNodeClass(ctx, name)
	env.CreateNodePoolWithExpiry(ctx, name, name, tc, expireAfter)
	env.CreateDeployment(ctx, name, name, name, tc.Arch)

	// Wait for the first pod to be running — the node is already expired at
	// this point so karpenter starts a replacement shortly after.
	firstPod := env.WaitForRunningPod(ctx, name)
	Expect(firstPod.Spec.NodeName).NotTo(BeEmpty())
	firstNodeName = firstPod.Spec.NodeName

	GinkgoWriter.Printf("[expiration] first node: %s; waiting for replacement...\n", firstNodeName)

	// Budget covers the full replacement cycle: karpenter creates a new
	// NodeClaim, provisions a new VM, pod migrates, old node is drained.
	replacementPod := env.WaitForPodOnDifferentNode(ctx, name, firstNodeName, environment.ReplacementTimeout)
	Expect(replacementPod.Spec.NodeName).NotTo(Equal(firstNodeName))

	GinkgoWriter.Printf("[expiration] replacement node: %s\n", replacementPod.Spec.NodeName)

	node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, replacementPod.Spec.NodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	Expect(environment.IsNodeReady(node)).To(BeTrue(), "replacement node %s is not Ready", node.Name)
	Expect(node.Labels[karpv1.NodePoolLabelKey]).To(Equal(name))

	// karpenter drains and terminates the expired node; no explicit test delete is needed.
	Expect(env.WaitForNodeRemoval(ctx, firstNodeName)).To(Succeed())
	firstNodeName = ""
}

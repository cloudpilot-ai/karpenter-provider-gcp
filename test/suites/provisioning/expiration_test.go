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

var _ = Describe("Expiration", func() {
	// Expiration test: set expireAfter on the NodePool so nodes are expired as
	// soon as they register (1m < typical provisioning time), then verify that
	// karpenter replaces the node with a fresh one and the pod migrates to it.
	It("should replace an expired amd64 on-demand node", func(ctx SpecContext) {
		runExpirationTest(ctx, provisioningCase{
			capacityType:  karpv1.CapacityTypeOnDemand,
			arch:          karpv1.ArchitectureAmd64,
			families:      []string{"e2", "n2"},
			instanceTypes: []string{"e2-medium", "e2-standard-2", "n2-standard-2"},
		})
	}, SpecTimeout(25*time.Minute))
})

func runExpirationTest(ctx context.Context, tc provisioningCase) {
	prefix := testPrefix(tc.arch, tc.capacityType) + "-exp"
	suffix := uniqueSuffix()
	nodeClassName := prefix + "-nc-" + suffix
	nodePoolName := prefix + "-np-" + suffix
	deployName := prefix + "-dep-" + suffix
	appLabel := prefix + "-" + suffix

	// expireAfter is set short enough that the node will already be expired
	// by the time it finishes bootstrapping (~5-7 min). Karpenter marks the
	// NodeClaim as Expired and provisions a replacement immediately.
	const expireAfter = "1m"

	GinkgoWriter.Printf("[setup] expiration arch=%s capacityType=%s nodePool=%s expireAfter=%s\n",
		tc.arch, tc.capacityType, nodePoolName, expireAfter)

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
	createNodePoolWithExpiry(ctx, nodePoolName, nodeClassName, tc, expireAfter)
	createDeployment(ctx, deployName, appLabel, nodePoolName, tc.arch)

	// Wait for the first pod to be running — the node is already expired at
	// this point so karpenter will start replacement shortly.
	firstPod := waitForRunningPod(ctx, appLabel)
	Expect(firstPod.Spec.NodeName).NotTo(BeEmpty())
	firstNodeName = firstPod.Spec.NodeName

	GinkgoWriter.Printf("[expiration] first node: %s; waiting for replacement...\n", firstNodeName)

	// Wait for the pod to be rescheduled onto a new node. Budget covers the
	// full replacement cycle: karpenter creates a new NodeClaim, provisions a
	// new GCP VM, pod migrates, and old node is drained.
	replacementTimeout := 2 * environment.ProvisioningTimeout
	replacementPod := waitForPodOnDifferentNode(ctx, appLabel, firstNodeName, replacementTimeout)
	Expect(replacementPod.Spec.NodeName).NotTo(Equal(firstNodeName))

	GinkgoWriter.Printf("[expiration] replacement node: %s\n", replacementPod.Spec.NodeName)

	// The replacement node must be Ready and karpenter-managed.
	node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, replacementPod.Spec.NodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	nodeReady := false
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
			nodeReady = true
		}
	}
	Expect(nodeReady).To(BeTrue(), "replacement node %s is not Ready", node.Name)
	Expect(node.Labels[karpv1.NodePoolLabelKey]).To(Equal(nodePoolName))

	// Original node must be gone (drained and terminated by karpenter).
	Expect(env.WaitForNodeRemoval(ctx, firstNodeName)).To(Succeed())
	firstNodeName = "" // already removed
}

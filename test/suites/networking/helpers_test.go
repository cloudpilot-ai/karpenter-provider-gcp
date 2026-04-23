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

package networking_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

// runPrivateNodeTest provisions a node via a GCENodeClass with
// networkConfig.enablePrivateNodes: true and asserts the resulting GCE instance
// has no access configs on its primary network interface (i.e. no external public IP).
func runPrivateNodeTest(ctx context.Context, tc environment.TestCase) {
	prefix := environment.TestPrefix(tc.Arch, tc.CapacityType, "networking")
	suffix := environment.UniqueSuffix()
	name := prefix + "-" + suffix

	GinkgoWriter.Printf("[setup] private-node arch=%s capacityType=%s nodePool=%s\n",
		tc.Arch, tc.CapacityType, name)

	var provisionedNodeName string
	DeferCleanup(func(ctx context.Context) {
		env.DeleteDeployment(ctx, name)
		env.DeleteNodePool(ctx, name)
		env.DeleteNodeClass(ctx, name)
		if provisionedNodeName != "" {
			Expect(env.WaitForNodeRemoval(ctx, provisionedNodeName)).To(Succeed())
		}
	})

	env.CreateNodeClassWithPrivateNetwork(ctx, name)
	env.CreateNodePool(ctx, name, name, tc)
	env.CreateDeployment(ctx, name, name, name, tc.Arch)

	pod := env.WaitForRunningPod(ctx, name)
	Expect(pod.Spec.NodeName).NotTo(BeEmpty())
	provisionedNodeName = pod.Spec.NodeName

	node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, provisionedNodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	Expect(node.Spec.ProviderID).NotTo(BeEmpty(), "node %s has no providerID", provisionedNodeName)

	instance, err := env.GetGCEInstance(ctx, node.Spec.ProviderID)
	Expect(err).NotTo(HaveOccurred(), "fetching GCE instance for node %s", provisionedNodeName)
	Expect(instance.NetworkInterfaces).NotTo(BeEmpty(), "GCE instance has no network interfaces")
	Expect(instance.NetworkInterfaces[0].AccessConfigs).To(BeEmpty(),
		"expected no access configs (no external IP) on primary interface of node %s", provisionedNodeName)
}

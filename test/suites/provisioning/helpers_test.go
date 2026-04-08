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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

// runProvisioningTest creates a NodePool/NodeClass/Deployment, waits for a pod to run
// on a newly provisioned node, and asserts the node carries the expected labels.
func runProvisioningTest(ctx context.Context, tc environment.TestCase) {
	prefix := environment.TestPrefix(tc.Arch, tc.CapacityType)
	suffix := environment.UniqueSuffix()
	nodeClassName := prefix + "-nc-" + suffix
	nodePoolName := prefix + "-np-" + suffix
	deployName := prefix + "-dep-" + suffix
	appLabel := prefix + "-" + suffix

	GinkgoWriter.Printf("[setup] arch=%s capacityType=%s nodePool=%s\n",
		tc.Arch, tc.CapacityType, nodePoolName)

	initialNodes := env.AllNodeNames(ctx)

	var provisionedNodeName string
	DeferCleanup(func(ctx context.Context) {
		env.DeleteDeployment(ctx, deployName)
		env.DeleteNodePool(ctx, nodePoolName)
		env.DeleteNodeClass(ctx, nodeClassName)
		if provisionedNodeName != "" {
			Expect(env.WaitForNodeRemoval(ctx, provisionedNodeName)).To(Succeed())
		}
	})

	env.CreateNodeClass(ctx, nodeClassName)
	env.CreateNodePool(ctx, nodePoolName, nodeClassName, tc)
	env.CreateDeployment(ctx, deployName, appLabel, nodePoolName, tc.Arch)

	pod := env.WaitForRunningPod(ctx, appLabel)
	Expect(pod.Spec.NodeName).NotTo(BeEmpty())

	node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	provisionedNodeName = node.Name

	_, existedBefore := initialNodes[node.Name]
	Expect(existedBefore).To(BeFalse(), "expected a newly provisioned node, got a pre-existing one")
	Expect(environment.IsNodeReady(node)).To(BeTrue(), "node %s is not Ready", node.Name)
	Expect(node.Labels[karpv1.NodeRegisteredLabelKey]).To(Equal("true"))
	Expect(node.Labels[karpv1.NodePoolLabelKey]).To(Equal(nodePoolName))
	Expect(node.Labels[karpv1.CapacityTypeLabelKey]).To(Equal(tc.CapacityType))
	Expect(node.Labels[corev1.LabelArchStable]).To(Equal(tc.Arch))
	Expect(tc.Families).To(ContainElement(node.Labels[gcpv1alpha1.LabelInstanceFamily]))
	Expect(tc.InstanceTypes).To(ContainElement(node.Labels[corev1.LabelInstanceTypeStable]))
}

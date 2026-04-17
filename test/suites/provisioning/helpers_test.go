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
	runProvisioningTestWithOptions(ctx, tc, false)
}

// runUbuntuProvisioningTest is like runProvisioningTest but creates a GCENodeClass
// with the Ubuntu image family and also validates that kube-proxy is Running on
// the provisioned node (Group 4 credential cross-pool reuse baseline).
func runUbuntuProvisioningTest(ctx context.Context, tc environment.TestCase) {
	runProvisioningTestWithOptions(ctx, tc, true)
}

// runProvisioningTestWithOptions is the shared implementation for provisioning tests.
// When ubuntu is true it creates a Ubuntu NodeClass and waits for kube-proxy to be running.
func runProvisioningTestWithOptions(ctx context.Context, tc environment.TestCase, ubuntu bool) {
	prefix := environment.TestPrefix(tc.Arch, tc.CapacityType, "provisioning")
	suffix := environment.UniqueSuffix()
	name := prefix + "-" + suffix

	GinkgoWriter.Printf("[setup] arch=%s capacityType=%s ubuntu=%v nodePool=%s\n",
		tc.Arch, tc.CapacityType, ubuntu, name)

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

	if ubuntu {
		env.CreateNodeClassWithUbuntu(ctx, name)
	} else {
		env.CreateNodeClass(ctx, name)
	}
	// Fail fast: if Karpenter can't resolve images the NodeClass never becomes
	// Ready, and the test would otherwise spin for the full ProvisioningTimeout.
	env.WaitForNodeClassReady(ctx, name)
	env.CreateNodePool(ctx, name, name, tc)
	env.CreateDeployment(ctx, name, name, name, tc.Arch)

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

	if ubuntu {
		// Validate Group 4 credential cross-pool reuse: kube-proxy must reach Running
		// using the source pool's KUBE_PROXY_TOKEN on a node that is not a member of
		// that pool.
		env.WaitForKubeProxyRunning(ctx, provisionedNodeName)
	}
}

/*
Copyright 2026 The CloudPilot AI Authors.

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

package kubelet_config_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

// kubeletConfigCase describes one e2e scenario: provision a node with
// kubeletConfig attached to the GCENodeClass, then assert against the
// resulting node's status. Mirrors environment.TestCase fields for the
// provisioning side; assertions live in assert.
type kubeletConfigCase struct {
	tc            environment.TestCase
	kubeletConfig map[string]any
	assert        func(node *corev1.Node)
}

var _ = DescribeTable("kubeletConfiguration honored on provisioned nodes (#398)",
	func(ctx SpecContext, c kubeletConfigCase) {
		runKubeletConfigTest(ctx, c)
	},

	// systemReserved reduces allocatable: the literal assertion from
	// issue #398. n2-standard-4 has 4 vCPU / 16 GiB. With systemReserved
	// cpu=1 / memory=1Gi, the kubelet must report allocatable < capacity
	// by at least those amounts.
	Entry("COS: systemReserved reduces allocatable", kubeletConfigCase{
		tc: environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2"},
			InstanceTypes: []string{"n2-standard-4"},
			ImageFamily:   gcpv1alpha1.ImageFamilyContainerOptimizedOS,
		},
		kubeletConfig: map[string]any{
			"systemReserved": map[string]any{
				"cpu":    "1",
				"memory": "1Gi",
			},
		},
		assert: assertSystemReservedReducesAllocatable,
	}, SpecTimeout(15*time.Minute)),

	Entry("Ubuntu: systemReserved reduces allocatable", kubeletConfigCase{
		tc: environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2"},
			InstanceTypes: []string{"n2-standard-4"},
			ImageFamily:   gcpv1alpha1.ImageFamilyUbuntu,
		},
		kubeletConfig: map[string]any{
			"systemReserved": map[string]any{
				"cpu":    "1",
				"memory": "1Gi",
			},
		},
		assert: assertSystemReservedReducesAllocatable,
	}, SpecTimeout(15*time.Minute)),

	// Partial kubeReserved override: issue #220 regression guard at the
	// integration level. The user sets only cpu; the kubelet must still
	// receive memory and ephemeral-storage from provider-computed
	// defaults so the node comes up Ready.
	Entry("COS: partial kubeReserved override keeps node Ready (#220)", kubeletConfigCase{
		tc: environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2"},
			InstanceTypes: []string{"n2-standard-4"},
			ImageFamily:   gcpv1alpha1.ImageFamilyContainerOptimizedOS,
		},
		kubeletConfig: map[string]any{
			"kubeReserved": map[string]any{
				"cpu": "500m",
			},
		},
		assert: assertKubeReservedCPUOverride,
	}, SpecTimeout(15*time.Minute)),

	// podsPerCore caps capacity[pods]. n2-standard-4 = 4 vCPU; with
	// podsPerCore=8 expect exactly 32.
	Entry("COS: podsPerCore caps capacity[pods]", kubeletConfigCase{
		tc: environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2"},
			InstanceTypes: []string{"n2-standard-4"},
			ImageFamily:   gcpv1alpha1.ImageFamilyContainerOptimizedOS,
		},
		kubeletConfig: map[string]any{
			"podsPerCore": int64(8),
		},
		assert: assertPodsPerCoreCapsCapacity(32),
	}, SpecTimeout(15*time.Minute)),

	// maxPods is reflected in capacity (scheduler / kubelet agreement,
	// issue #133). maxPods=50 is a value clearly distinct from common
	// defaults, so checking node.status.capacity[pods] proves the setting
	// was propagated to kubelet.
	Entry("COS: maxPods reflected in capacity (#133)", kubeletConfigCase{
		tc: environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2"},
			InstanceTypes: []string{"n2-standard-4"},
			ImageFamily:   gcpv1alpha1.ImageFamilyContainerOptimizedOS,
		},
		kubeletConfig: map[string]any{
			"maxPods": int64(50),
		},
		assert: assertMaxPodsCapacity(50),
	}, SpecTimeout(15*time.Minute)),
)

// runKubeletConfigTest provisions a node with the given kubeletConfig and
// invokes c.assert against the resulting Node. Mirrors
// provisioning_test.runProvisioningTest but uses
// CreateNodeClassWithKubeletConfig.
func runKubeletConfigTest(ctx context.Context, c kubeletConfigCase) {
	prefix := environment.TestPrefix(c.tc.Arch, c.tc.CapacityType, osSlug(c.tc.ImageFamily), "kubeletconfig")
	suffix := environment.UniqueSuffix()
	name := prefix + "-" + suffix

	GinkgoWriter.Printf("[setup] arch=%s capacityType=%s os=%s nodePool=%s kubeletConfig=%v\n",
		c.tc.Arch, c.tc.CapacityType, c.tc.ImageFamily, name, c.kubeletConfig)

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

	imageFamily := c.tc.ImageFamily
	if imageFamily == "" {
		imageFamily = gcpv1alpha1.ImageFamilyContainerOptimizedOS
	}
	env.CreateNodeClassWithKubeletConfig(ctx, name, imageFamily, c.kubeletConfig)
	env.WaitForNodeClassReady(ctx, name)
	env.CreateNodePool(ctx, name, name, c.tc)
	env.WaitForNodePoolReady(ctx, name)
	env.CreateDeployment(ctx, name, name, name, c.tc.Arch)

	env.WaitForNodeClaimLaunched(ctx, name)
	pod := env.WaitForRunningPod(ctx, name)
	Expect(pod.Spec.NodeName).NotTo(BeEmpty())

	node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	provisionedNodeName = node.Name

	_, existedBefore := initialNodes[node.Name]
	Expect(existedBefore).To(BeFalse(), "expected a newly provisioned node, got a pre-existing one")
	Expect(environment.IsNodeReady(node)).To(BeTrue(), "node %s is not Ready", node.Name)
	Expect(c.tc.InstanceTypes).To(ContainElement(node.Labels[corev1.LabelInstanceTypeStable]))

	c.assert(node)
}

// assertSystemReservedReducesAllocatable verifies that the user-configured
// systemReserved (cpu=1, memory=1Gi) is reflected in node.status.allocatable.
// Memory is asserted leniently: allocatable.memory < capacity.memory - 1Gi,
// because eviction and provider-computed kubeReserved also reduce
// allocatable. CPU is asserted strictly: allocatable.cpu ≤ capacity.cpu - 1.
func assertSystemReservedReducesAllocatable(node *corev1.Node) {
	capCPU := node.Status.Capacity[corev1.ResourceCPU]
	allocCPU := node.Status.Allocatable[corev1.ResourceCPU]
	oneCPU := resource.MustParse("1")
	// expected upper bound on allocatable: capacity - 1 CPU
	capMinusOne := capCPU.DeepCopy()
	capMinusOne.Sub(oneCPU)
	Expect(allocCPU.Cmp(capMinusOne) <= 0).To(BeTrue(),
		"allocatable cpu %s must be <= capacity %s - 1 CPU (= %s) when systemReserved.cpu=1",
		allocCPU.String(), capCPU.String(), capMinusOne.String())

	capMem := node.Status.Capacity[corev1.ResourceMemory]
	allocMem := node.Status.Allocatable[corev1.ResourceMemory]
	oneGi := resource.MustParse("1Gi")
	capMinusOneGi := capMem.DeepCopy()
	capMinusOneGi.Sub(oneGi)
	Expect(allocMem.Cmp(capMinusOneGi) < 0).To(BeTrue(),
		"allocatable memory %s must be strictly less than capacity %s - 1Gi (= %s) when systemReserved.memory=1Gi",
		allocMem.String(), capMem.String(), capMinusOneGi.String())
}

// assertKubeReservedCPUOverride verifies that a partial kubeReserved (only
// cpu=500m set by the user) keeps the node healthy and reduces allocatable
// cpu by at least 500m. The "node is Ready" check inside runKubeletConfigTest
// is the #220 integration regression guard — proves the kubelet did not
// receive a partial kubeReserved that crashed startup.
func assertKubeReservedCPUOverride(node *corev1.Node) {
	capCPU := node.Status.Capacity[corev1.ResourceCPU]
	allocCPU := node.Status.Allocatable[corev1.ResourceCPU]
	halfCPU := resource.MustParse("500m")
	capMinusHalf := capCPU.DeepCopy()
	capMinusHalf.Sub(halfCPU)
	Expect(allocCPU.Cmp(capMinusHalf) <= 0).To(BeTrue(),
		"allocatable cpu %s must be <= capacity %s - 500m (= %s) when kubeReserved.cpu=500m",
		allocCPU.String(), capCPU.String(), capMinusHalf.String())
}

// assertPodsPerCoreCapsCapacity returns an assertion that
// node.status.capacity[pods] equals the given count.
func assertPodsPerCoreCapsCapacity(want int64) func(*corev1.Node) {
	return func(node *corev1.Node) {
		pods := node.Status.Capacity[corev1.ResourcePods]
		Expect(pods.Value()).To(Equal(want),
			"capacity.pods %s must equal podsPerCore × cpus", pods.String())
	}
}

// assertMaxPodsCapacity returns an assertion that node.status.capacity[pods]
// equals the user-configured maxPods. Pinning the value is the #133 regression
// guard against kubelet / scheduler disagreement.
func assertMaxPodsCapacity(want int64) func(*corev1.Node) {
	return func(node *corev1.Node) {
		pods := node.Status.Capacity[corev1.ResourcePods]
		Expect(pods.Value()).To(Equal(want),
			"capacity.pods %s must equal kubeletConfiguration.maxPods", pods.String())
	}
}

func osSlug(imageFamily string) string {
	if imageFamily == gcpv1alpha1.ImageFamilyUbuntu {
		return "ubuntu"
	}
	return "cos"
}

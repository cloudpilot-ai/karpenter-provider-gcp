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

package environment

import (
	"context"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

// RunProvisioningTest provisions a node and validates it against the test case.
func (e *Environment) RunProvisioningTest(ctx context.Context, tc TestCase) {
	prefix := TestPrefix(tc.Arch, tc.CapacityType, osSlug(tc.ImageFamily), "provisioning")
	suffix := UniqueSuffix()
	name := prefix + "-" + suffix

	GinkgoWriter.Printf("[setup] arch=%s capacityType=%s os=%s nodePool=%s\n",
		tc.Arch, tc.CapacityType, tc.ImageFamily, name)

	initialNodes := e.AllNodeNames(ctx)

	var provisionedNodeName string
	DeferCleanup(func(ctx context.Context) {
		e.DeleteDeployment(ctx, name)
		e.DeleteNodePool(ctx, name)
		e.DeleteNodeClass(ctx, name)
		if provisionedNodeName != "" {
			Expect(e.WaitForNodeRemoval(ctx, provisionedNodeName)).To(Succeed())
		}
	})

	imageFamily := tc.ImageFamily
	if imageFamily == "" {
		imageFamily = gcpv1alpha1.ImageFamilyContainerOptimizedOS
	}
	if tc.LocalSSDMode != "" {
		e.CreateNodeClassForLocalSSD(ctx, name, imageFamily, tc.BootDiskCategory, tc.LocalSSDMode)
	} else {
		e.CreateNodeClass(ctx, name, imageFamily)
	}
	e.WaitForNodeClassReady(ctx, name)
	e.CreateNodePool(ctx, name, name, tc)
	e.WaitForNodePoolReady(ctx, name)
	deployOpts := DeploymentOptions{}
	if tc.PodLocalSSDCount != "" {
		deployOpts.ExtraNodeSelectors = map[string]string{
			gcpv1alpha1.LabelInstanceLocalSsdCount: tc.PodLocalSSDCount,
		}
	}
	if tc.PodEphemeralStorage != "" {
		deployOpts.EphemeralStorageRequest = tc.PodEphemeralStorage
	}
	e.CreateDeploymentWithOptions(ctx, name, name, name, tc.Arch, deployOpts)

	e.WaitForNodeClaimLaunched(ctx, name)
	pod := e.WaitForRunningPod(ctx, name)
	Expect(pod.Spec.NodeName).NotTo(BeEmpty())

	node, err := e.KubeClient.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	provisionedNodeName = node.Name

	_, existedBefore := initialNodes[node.Name]
	Expect(existedBefore).To(BeFalse(), "expected a newly provisioned node, got a pre-existing one")
	Expect(IsNodeReady(node)).To(BeTrue(), "node %s is not Ready", node.Name)
	Expect(node.Labels[karpv1.NodeRegisteredLabelKey]).To(Equal("true"))
	Expect(node.Labels[karpv1.NodePoolLabelKey]).To(Equal(name))
	Expect(node.Labels[karpv1.CapacityTypeLabelKey]).To(Equal(tc.CapacityType))
	Expect(node.Labels[corev1.LabelArchStable]).To(Equal(tc.Arch))
	Expect(tc.Families).To(ContainElement(node.Labels[gcpv1alpha1.LabelInstanceFamily]))
	Expect(tc.InstanceTypes).To(ContainElement(node.Labels[corev1.LabelInstanceTypeStable]))

	expectedScratch := expectedScratchDiskCount(tc)
	inst, err := e.GetGCEInstance(ctx, node.Spec.ProviderID)
	Expect(err).NotTo(HaveOccurred(), "fetching GCE instance for %s", node.Name)
	var scratch int
	for _, d := range inst.Disks {
		if d.Type == "SCRATCH" && d.Interface == "NVME" {
			scratch++
		}
	}
	Expect(scratch).To(Equal(expectedScratch),
		"expected %d local SSDs on %s, got %d", expectedScratch, node.Name, scratch)

	if expectedScratch > 0 {
		// Bootstrap labels can appear after the Pod starts.
		labelKey := "cloud.google.com/gke-local-nvme-ssd"
		if tc.LocalSSDMode == gcpv1alpha1.LocalSSDModeEphemeral {
			labelKey = "cloud.google.com/gke-ephemeral-storage-local-ssd"
		}
		Eventually(func(g Gomega) {
			n, err := e.KubeClient.CoreV1().Nodes().Get(ctx, provisionedNodeName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(n.Labels[labelKey]).To(Equal("true"),
				"expected %s=true node label", labelKey)
		}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
	}

	expectedCountLabel := expectedSSDCountLabelValue(tc)
	Eventually(func(g Gomega) {
		n, err := e.KubeClient.CoreV1().Nodes().Get(ctx, provisionedNodeName, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		got := n.Labels[gcpv1alpha1.LabelInstanceLocalSsdCount]
		g.Expect(got).To(Equal(expectedCountLabel),
			"node %s: %s = %q, want %q",
			provisionedNodeName, gcpv1alpha1.LabelInstanceLocalSsdCount, got, expectedCountLabel)
	}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

	e.assertEphemeralStorageAllocatable(ctx, provisionedNodeName, tc)

	e.WaitForKubeProxyRunning(ctx, provisionedNodeName)
}

func (e *Environment) assertEphemeralStorageAllocatable(ctx context.Context, nodeName string, tc TestCase) {
	if tc.PodEphemeralStorage == "" {
		return
	}
	req := resource.MustParse(tc.PodEphemeralStorage)
	n, err := e.KubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	alloc := n.Status.Allocatable[corev1.ResourceEphemeralStorage]
	Expect(alloc.Cmp(req)).To(BeNumerically(">=", 0),
		"node %s: ephemeral-storage allocatable %s < requested %s",
		nodeName, alloc.String(), req.String())
}

func osSlug(imageFamily string) string {
	if imageFamily == gcpv1alpha1.ImageFamilyUbuntu {
		return "ubuntu"
	}
	return "cos"
}

func expectedSSDCountLabelValue(tc TestCase) string {
	switch {
	case tc.PodLocalSSDCount != "":
		return tc.PodLocalSSDCount
	case tc.ExpectedScratchDisks > 0:
		return strconv.Itoa(tc.ExpectedScratchDisks)
	default:
		return "0"
	}
}

func expectedScratchDiskCount(tc TestCase) int {
	switch {
	case tc.ExpectedScratchDisks > 0:
		return tc.ExpectedScratchDisks
	case tc.PodLocalSSDCount != "":
		n, err := strconv.Atoi(tc.PodLocalSSDCount)
		if err != nil || n < 0 {
			return 0
		}
		return n
	default:
		return 0
	}
}

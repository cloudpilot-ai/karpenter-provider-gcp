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

package gpu

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

var env *environment.Environment
var _ = BeforeEach(func() { env = environment.Current() })

var _ = Describe("GPU Node", Label("suite:gpu"), func() {
	// This single It covers all GPU provisioning invariants on one node to avoid
	// spinning up multiple expensive GPU instances.
	It("should provision a GPU node with correct taint and labels and run two pods using GPU time-sharing", func(ctx SpecContext) {
		prefix := environment.TestPrefix(karpv1.ArchitectureAmd64, karpv1.CapacityTypeOnDemand, "gpu-node")
		suffix := environment.UniqueSuffix()
		name := prefix + "-" + suffix

		GinkgoWriter.Printf("[setup] gpu-node nodePool=%s\n", name)

		DeferCleanup(func(cleanupCtx SpecContext) {
			env.DeleteDeployment(cleanupCtx, name)
			env.DeleteNodePool(cleanupCtx, name)
			env.DeleteNodeClass(cleanupCtx, name)
		})

		env.CreateNodeClassWithAutoGPUTaint(ctx, name, "default", &gcpv1alpha1.GPUSharingConfig{
			Strategy:               gcpv1alpha1.GPUSharingStrategyTimeSharing,
			MaxSharedClientsPerGPU: 2,
		})
		env.WaitForNodeClassReady(ctx, name)

		env.CreateNodePool(ctx, name, name, environment.TestCase{
			CapacityType:        karpv1.CapacityTypeOnDemand,
			Arch:                karpv1.ArchitectureAmd64,
			Families:            []string{"g2"},
			GPUCount:            "1",
			ImageFamily:         gcpv1alpha1.ImageFamilyContainerOptimizedOS,
			ConsolidationPolicy: "WhenEmpty",
		})
		env.WaitForNodePoolReady(ctx, name)

		sharingLabels := map[string]string{
			gcpv1alpha1.LabelGKEGPUSharingStrategy:     "time-sharing",
			gcpv1alpha1.LabelGKEMaxSharedClientsPerGPU: "2",
		}
		env.CreateDeploymentOnGPUNode(ctx, name, name, name, 2, sharingLabels)
		env.WaitForNodeClaimLaunched(ctx, name)
		env.WaitForNodeClaimInitialized(ctx, name)
		pod := env.WaitForRunningPod(ctx, name)
		Expect(pod.Spec.NodeName).NotTo(BeEmpty())

		By("waiting for both shared-GPU pods to be running and ready on the same node")
		Eventually(func(g Gomega) {
			pods, err := env.KubeClient.CoreV1().Pods(environment.TestNamespace).List(ctx,
				metav1.ListOptions{LabelSelector: "app=" + name})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pods.Items).To(HaveLen(2))
			for _, sharedPod := range pods.Items {
				g.Expect(sharedPod.DeletionTimestamp).To(BeNil())
				g.Expect(sharedPod.Spec.NodeName).To(Equal(pod.Spec.NodeName))
				g.Expect(sharedPod.Status.Phase).To(Equal(corev1.PodRunning))
				g.Expect(sharedPod.Status.Conditions).To(ContainElement(And(
					HaveField("Type", corev1.PodReady),
					HaveField("Status", corev1.ConditionTrue),
				)))
			}
		}).WithTimeout(environment.ProvisioningTimeout).WithPolling(environment.DefaultPollInterval).Should(Succeed())

		node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("asserting the device plugin advertises two shared GPUs")
		capacity := node.Status.Capacity["nvidia.com/gpu"]
		allocatable := node.Status.Allocatable["nvidia.com/gpu"]
		Expect(capacity.Value()).To(Equal(int64(2)))
		Expect(allocatable.Value()).To(Equal(int64(2)))
		for key, value := range sharingLabels {
			Expect(node.Labels).To(HaveKeyWithValue(key, value))
		}

		By("asserting autoGPUTaint: node has nvidia.com/gpu=present:NoSchedule taint")
		var gpuTaint *corev1.Taint
		for i := range node.Spec.Taints {
			t := &node.Spec.Taints[i]
			if t.Key == "nvidia.com/gpu" && t.Value == "present" && t.Effect == corev1.TaintEffectNoSchedule {
				gpuTaint = t
				break
			}
		}
		Expect(gpuTaint).NotTo(BeNil(), "node %s should have nvidia.com/gpu=present:NoSchedule taint", node.Name)

		By("asserting gpuDriverVersion: node has cloud.google.com/gke-gpu-driver-version=default label")
		Expect(node.Labels).To(HaveKeyWithValue(
			gcpv1alpha1.LabelGKEGPUDriverVersion, "default",
		), "node %s should carry gke-gpu-driver-version=default", node.Name)

		By("asserting gke-accelerator: node has cloud.google.com/gke-accelerator label with non-empty value")
		accelerator, ok := node.Labels[gcpv1alpha1.LabelGKEAccelerator]
		Expect(ok).To(BeTrue(), "node %s should have gke-accelerator label", node.Name)
		Expect(accelerator).NotTo(BeEmpty(), "gke-accelerator label must be non-empty on node %s", node.Name)
	}, SpecTimeout(45*time.Minute))
})

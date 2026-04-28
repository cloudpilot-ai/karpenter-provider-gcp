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

package gpu_test

import (
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

// GPU tests consume real GPU quota. Run only when E2E_GPU_TESTS=true.
var _ = Describe("GPU Auto-Taint", func() {
	BeforeEach(func() {
		if os.Getenv("E2E_GPU_TESTS") != "true" {
			Skip("set E2E_GPU_TESTS=true to run GPU quota tests")
		}
	})

	It("should provision a GPU node with nvidia.com/gpu=present:NoSchedule taint when autoGPUTaint=true", func(ctx SpecContext) {
		prefix := environment.TestPrefix(karpv1.ArchitectureAmd64, karpv1.CapacityTypeOnDemand, "gpu-taint")
		suffix := environment.UniqueSuffix()
		name := prefix + "-" + suffix

		GinkgoWriter.Printf("[setup] gpu-taint nodePool=%s\n", name)

		DeferCleanup(func(cleanupCtx SpecContext) {
			env.DeleteDeployment(cleanupCtx, name)
			env.DeleteNodePool(cleanupCtx, name)
			env.DeleteNodeClass(cleanupCtx, name)
		})

		env.CreateNodeClassWithAutoGPUTaint(ctx, name)
		env.WaitForNodeClassReady(ctx, name)

		env.CreateNodePool(ctx, name, name, environment.TestCase{
			CapacityType:        karpv1.CapacityTypeOnDemand,
			Arch:                karpv1.ArchitectureAmd64,
			Families:            []string{"g2"},
			InstanceTypes:       []string{"g2-standard-4"},
			ImageFamily:         gcpv1alpha1.ImageFamilyContainerOptimizedOS,
			ConsolidationPolicy: "WhenEmpty",
		})
		env.WaitForNodePoolReady(ctx, name)

		env.CreateDeploymentWithGPU(ctx, name, name, name)
		env.WaitForNodeClaimLaunched(ctx, name)
		env.WaitForNodeClaimInitialized(ctx, name)
		pod := env.WaitForRunningPod(ctx, name)
		Expect(pod.Spec.NodeName).NotTo(BeEmpty())

		node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())

		var gpuTaint *corev1.Taint
		for i := range node.Spec.Taints {
			t := &node.Spec.Taints[i]
			if t.Key == "nvidia.com/gpu" && t.Value == "present" && t.Effect == corev1.TaintEffectNoSchedule {
				gpuTaint = t
				break
			}
		}
		Expect(gpuTaint).NotTo(BeNil(), "node %s should have nvidia.com/gpu=present:NoSchedule taint", node.Name)
	}, SpecTimeout(35*time.Minute))
})

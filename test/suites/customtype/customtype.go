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

// Package customtype is an end-to-end regression test for issue #144: it registers a
// GCECustomMachineType (proposals/0009-custom-machine-type-catalog.md), waits for the
// gcecustommachinetype controller to resolve it against the live cluster via machineTypes.get,
// and verifies a workload actually schedules onto that exact custom shape rather than a
// predefined one, closing the gap the unit tests can only simulate against fakes.
package customtype

import (
	"context"
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

var _ = Describe("Custom Machine Type", Label("suite:customtype"), func() {
	It("should schedule a workload onto a registered custom machine type", func(ctx SpecContext) {
		// n2-custom-4-20480: 4 vCPU / 20 GiB (5 GiB/vCPU). Deliberately not 4 GiB/vCPU
		// (n2-standard-4), 8 GiB/vCPU (n2-highmem-4), or 1 GiB/vCPU (n2-highcpu-4): GCP
		// collapses a custom shape that exactly matches a predefined ratio into that
		// predefined machine type, so the created instance (and its instance-type label)
		// would come back as the predefined name instead - see proposals/0009's Open
		// Questions. A genuinely off-ratio shape is required to test custom-type scheduling.
		const machineType = "n2-custom-4-20480"

		prefix := environment.TestPrefix(karpv1.ArchitectureAmd64, karpv1.CapacityTypeOnDemand, "customtype")
		name := prefix + "-" + environment.UniqueSuffix()

		GinkgoWriter.Printf("[setup] machineType=%s registration=%s nodePool=%s\n", machineType, name, name)

		initialNodes := env.AllNodeNames(ctx)

		var provisionedNodeName string
		DeferCleanup(func(ctx context.Context) {
			env.DeleteDeployment(ctx, name)
			env.DeleteNodePool(ctx, name)
			env.DeleteNodeClass(ctx, name)
			env.DeleteCustomMachineType(ctx, name)
			if provisionedNodeName != "" {
				Expect(env.WaitForNodeRemoval(ctx, provisionedNodeName)).To(Succeed())
			}
		})

		env.CreateCustomMachineType(ctx, name, machineType, "0.10", "0.03")
		env.WaitForCustomMachineTypeReady(ctx, name)

		env.CreateNodeClass(ctx, name, gcpv1alpha1.ImageFamilyContainerOptimizedOS)
		env.WaitForNodeClassReady(ctx, name)
		env.CreateNodePool(ctx, name, name, environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2"},
			InstanceTypes: []string{machineType},
		})
		env.WaitForNodePoolReady(ctx, name)
		env.CreateDeployment(ctx, name, name, name, karpv1.ArchitectureAmd64)

		env.WaitForNodeClaimLaunched(ctx, name)
		pod := env.WaitForRunningPod(ctx, name)
		Expect(pod.Spec.NodeName).NotTo(BeEmpty())

		node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		provisionedNodeName = node.Name

		_, existedBefore := initialNodes[node.Name]
		Expect(existedBefore).To(BeFalse(), "expected a newly provisioned node, got a pre-existing one")
		Expect(environment.IsNodeReady(node)).To(BeTrue(), "node %s is not Ready", node.Name)
		Expect(node.Labels[karpv1.NodePoolLabelKey]).To(Equal(name))
		Expect(node.Labels[corev1.LabelInstanceTypeStable]).To(Equal(machineType),
			"node must be scheduled onto the registered custom machine type, not a predefined shape")
		Expect(node.Labels[gcpv1alpha1.LabelInstanceFamily]).To(Equal("n2"))

		env.WaitForKubeProxyRunning(ctx, provisionedNodeName)
	}, SpecTimeout(15*time.Minute))
})

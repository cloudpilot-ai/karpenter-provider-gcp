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

package nestedvirtualization

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

var env *environment.Environment
var _ = BeforeEach(func() { env = environment.Current() })

var _ = Describe("Nested Virtualization", Label("suite:nested-virtualization"), func() {
	It("should provision an N2 node with nested virtualization enabled", func(ctx SpecContext) {
		prefix := environment.TestPrefix(karpv1.ArchitectureAmd64, karpv1.CapacityTypeOnDemand, "nested-virt")
		name := prefix + "-" + environment.UniqueSuffix()

		GinkgoWriter.Printf("[setup] nested virtualization nodePool=%s\n", name)

		var provisionedNodeName string
		DeferCleanup(func(ctx context.Context) {
			env.DeleteDeployment(ctx, name)
			env.DeleteNodePool(ctx, name)
			env.DeleteNodeClass(ctx, name)
			if provisionedNodeName != "" {
				Expect(env.WaitForNodeRemoval(ctx, provisionedNodeName)).To(Succeed())
			}
		})

		env.CreateNodeClassWithNestedVirtualization(ctx, name)
		env.CreateNodePool(ctx, name, name, environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2"},
			InstanceTypes: []string{"n2-standard-2", "n2-standard-4"},
		})
		env.CreateDeployment(ctx, name, name, name, karpv1.ArchitectureAmd64)

		pod := env.WaitForRunningPod(ctx, name)
		Expect(pod.Spec.NodeName).NotTo(BeEmpty())
		provisionedNodeName = pod.Spec.NodeName

		node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, provisionedNodeName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(node.Spec.ProviderID).NotTo(BeEmpty(), "node %s has no providerID", provisionedNodeName)

		instance, err := env.GetGCEInstance(ctx, node.Spec.ProviderID)
		Expect(err).NotTo(HaveOccurred(), "fetching GCE instance for node %s", provisionedNodeName)
		Expect(instance.AdvancedMachineFeatures).NotTo(BeNil(),
			"GCE instance for node %s has no AdvancedMachineFeatures", provisionedNodeName)
		Expect(instance.AdvancedMachineFeatures.EnableNestedVirtualization).To(BeTrue(),
			"EnableNestedVirtualization should be true on node %s", provisionedNodeName)
	}, SpecTimeout(15*time.Minute))
})

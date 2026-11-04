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

package threadspercore

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

var env *environment.Environment
var _ = BeforeEach(func() { env = environment.Current() })

var _ = Describe("Threads Per Core", Label("suite:threads-per-core"), func() {
	// n2-standard-4 has 4 vCPUs and runs two threads per core. With one thread
	// per core the guest sees 2 CPUs while GCE still bills 4, so Karpenter must
	// plan against 2. podsPerCore turns the CPU count into a pod limit the
	// kubelet enforces, which checks the count the node really reports.
	const (
		machineType     = "n2-standard-4"
		wantCPU         = int64(2)
		podsPerCore     = int64(5)
		wantPods        = int64(10)
		wantThreadsCore = int64(1)
	)

	It("should size the node by the CPUs it reports when SMT is disabled", func(ctx SpecContext) {
		prefix := environment.TestPrefix(karpv1.ArchitectureAmd64, karpv1.CapacityTypeOnDemand, "threads")
		name := prefix + "-" + environment.UniqueSuffix()

		GinkgoWriter.Printf("[setup] threads-per-core nodePool=%s\n", name)

		var provisionedNodeName string
		DeferCleanup(func(ctx context.Context) {
			env.DeleteDeployment(ctx, name)
			env.DeleteNodePool(ctx, name)
			env.DeleteNodeClass(ctx, name)
			if provisionedNodeName != "" {
				Expect(env.WaitForNodeRemoval(ctx, provisionedNodeName)).To(Succeed())
			}
		})

		env.CreateNodeClassWithThreadsPerCore(ctx, name, wantThreadsCore, podsPerCore)
		env.CreateNodePool(ctx, name, name, environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2"},
			InstanceTypes: []string{machineType},
		})
		env.CreateDeployment(ctx, name, name, name, karpv1.ArchitectureAmd64)

		pod := env.WaitForRunningPod(ctx, name)
		Expect(pod.Spec.NodeName).NotTo(BeEmpty())
		provisionedNodeName = pod.Spec.NodeName

		node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, provisionedNodeName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(node.Spec.ProviderID).NotTo(BeEmpty(), "node %s has no providerID", provisionedNodeName)

		By("GCE records one thread per core")
		instance, err := env.GetGCEInstance(ctx, node.Spec.ProviderID)
		Expect(err).NotTo(HaveOccurred(), "fetching GCE instance for node %s", provisionedNodeName)
		Expect(instance.AdvancedMachineFeatures).NotTo(BeNil(),
			"GCE instance for node %s has no AdvancedMachineFeatures", provisionedNodeName)
		Expect(instance.AdvancedMachineFeatures.ThreadsPerCore).To(Equal(wantThreadsCore),
			"ThreadsPerCore on node %s", provisionedNodeName)

		By("the node reports half the machine type's vCPUs and the pod limit that follows")
		nodeCPU := node.Status.Capacity[corev1.ResourceCPU]
		Expect(nodeCPU.Value()).To(Equal(wantCPU), "node %s capacity cpu = %s", provisionedNodeName, nodeCPU.String())
		nodePods := node.Status.Capacity[corev1.ResourcePods]
		Expect(nodePods.Value()).To(Equal(wantPods), "node %s capacity pods = %s", provisionedNodeName, nodePods.String())

		By("the NodeClaim was planned against the same CPUs")
		var claim *unstructured.Unstructured
		Eventually(func(g Gomega) {
			var err error
			claim, err = env.GetNodeClaimForNode(ctx, provisionedNodeName)
			g.Expect(err).NotTo(HaveOccurred())
		}).WithTimeout(time.Minute).WithPolling(5 * time.Second).Should(Succeed())

		claimCapacity := quantity(claim, "status", "capacity", "cpu")
		Expect(claimCapacity.Cmp(nodeCPU)).To(BeZero(),
			"NodeClaim capacity cpu %s != node capacity cpu %s", claimCapacity.String(), nodeCPU.String())
		claimPods := quantity(claim, "status", "capacity", "pods")
		Expect(claimPods.Cmp(nodePods)).To(BeZero(),
			"NodeClaim capacity pods %s != node capacity pods %s", claimPods.String(), nodePods.String())

		// Allocatable is not compared for equality: Karpenter counts the kube-proxy
		// mirror pod's CPU as provider overhead that the node does not subtract.
		// Both must still sit under the visible CPUs; without the fix the
		// NodeClaim's would be near 4.
		wantCap := *resource.NewQuantity(wantCPU, resource.DecimalSI)
		nodeAllocatable := node.Status.Allocatable[corev1.ResourceCPU]
		Expect(nodeAllocatable.Cmp(wantCap)).To(BeNumerically("<=", 0),
			"node allocatable cpu %s exceeds %s visible CPUs", nodeAllocatable.String(), wantCap.String())
		claimAllocatable := quantity(claim, "status", "allocatable", "cpu")
		Expect(claimAllocatable.Cmp(wantCap)).To(BeNumerically("<=", 0),
			"NodeClaim allocatable cpu %s exceeds %s visible CPUs", claimAllocatable.String(), wantCap.String())
	}, SpecTimeout(15*time.Minute))
})

// quantity reads a resource quantity from an unstructured object's nested path.
func quantity(obj *unstructured.Unstructured, fields ...string) resource.Quantity {
	raw, found, err := unstructured.NestedString(obj.Object, fields...)
	Expect(err).NotTo(HaveOccurred())
	Expect(found).To(BeTrue(), "%s not set on %s", fields, obj.GetName())
	return resource.MustParse(raw)
}

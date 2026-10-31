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

package instanceconfig

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

var _ = Describe("Instance Config", Label("suite:instance-config"), func() {
	// Cascade Lake is a floor that every N2 zone offers. GCE reports the platform
	// that was requested, so this checks the field reached the instance, not which
	// platform the scheduler picked.
	const (
		nicType        = "GVNIC"
		minCpuPlatform = "Intel Cascade Lake"
	)

	It("should apply nicType and minCpuPlatform to the GCE instance", func(ctx SpecContext) {
		prefix := environment.TestPrefix(karpv1.ArchitectureAmd64, karpv1.CapacityTypeOnDemand, "inst-cfg")
		name := prefix + "-" + environment.UniqueSuffix()

		GinkgoWriter.Printf("[setup] instance-config nodePool=%s\n", name)

		var provisionedNodeName string
		DeferCleanup(func(ctx context.Context) {
			env.DeleteDeployment(ctx, name)
			env.DeleteNodePool(ctx, name)
			env.DeleteNodeClass(ctx, name)
			if provisionedNodeName != "" {
				Expect(env.WaitForNodeRemoval(ctx, provisionedNodeName)).To(Succeed())
			}
		})

		env.CreateNodeClassWithNicTypeAndMinCpuPlatform(ctx, name, nicType, minCpuPlatform)
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

		By("every interface has the requested type")
		Expect(instance.NetworkInterfaces).NotTo(BeEmpty(), "node %s has no network interfaces", provisionedNodeName)
		for i, iface := range instance.NetworkInterfaces {
			Expect(iface.NicType).To(Equal(nicType), "interface %d on node %s", i, provisionedNodeName)
		}

		By("the requested minimum CPU platform is recorded")
		Expect(instance.MinCpuPlatform).To(Equal(minCpuPlatform), "MinCpuPlatform on node %s", provisionedNodeName)
	}, SpecTimeout(15*time.Minute))
})

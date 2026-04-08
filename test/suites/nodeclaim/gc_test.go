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

package nodeclaim_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

// gcTimeout is the maximum time to wait for the GC controller to detect and
// delete an orphaned VM. The controller re-queues every 2 minutes; we allow
// two full cycles plus VM deletion time as a comfortable margin.
const gcTimeout = 6 * time.Minute

var _ = Describe("GarbageCollection", func() {
	XIt("should delete an orphaned VM after its NodeClaim is force-removed", func(ctx SpecContext) {
		runGCTest(ctx, environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2"},
			InstanceTypes: []string{"n2-standard-2", "n2-standard-4"},
		})
	}, SpecTimeout(environment.ProvisioningTimeout+gcTimeout+environment.NodeCleanupTimeout))
})

func runGCTest(ctx context.Context, tc environment.TestCase) {
	prefix := environment.TestPrefix(tc.Arch, tc.CapacityType) + "-gc"
	suffix := environment.UniqueSuffix()
	nodeClassName := prefix + "-nc-" + suffix
	nodePoolName := prefix + "-np-" + suffix
	deployName := prefix + "-dep-" + suffix
	appLabel := prefix + "-" + suffix

	GinkgoWriter.Printf("[setup] gc arch=%s capacityType=%s nodePool=%s\n",
		tc.Arch, tc.CapacityType, nodePoolName)

	var (
		provisionedNodeName string
		providerID          string
	)
	DeferCleanup(func(ctx context.Context) {
		env.DeleteDeployment(ctx, deployName)
		env.DeleteNodePool(ctx, nodePoolName)
		env.DeleteNodeClass(ctx, nodeClassName)
		if provisionedNodeName != "" {
			_ = env.WaitForNodeRemoval(ctx, provisionedNodeName)
		}
	})

	env.CreateNodeClass(ctx, nodeClassName)
	env.CreateNodePool(ctx, nodePoolName, nodeClassName, tc)
	env.CreateDeployment(ctx, deployName, appLabel, nodePoolName, tc.Arch)

	pod := env.WaitForRunningPod(ctx, appLabel)
	Expect(pod.Spec.NodeName).NotTo(BeEmpty())
	provisionedNodeName = pod.Spec.NodeName

	node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, provisionedNodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	providerID = node.Spec.ProviderID
	Expect(providerID).NotTo(BeEmpty(), "node %s has no providerID", provisionedNodeName)

	GinkgoWriter.Printf("[gc] node=%s providerID=%s\n", provisionedNodeName, providerID)

	claims, err := env.ListNodeClaims(ctx)
	Expect(err).NotTo(HaveOccurred())
	var nodeClaimName string
	for _, c := range claims {
		pid, _, _ := unstructured.NestedString(c.Object, "status", "providerID")
		if pid == providerID {
			nodeClaimName = c.GetName()
			break
		}
	}
	Expect(nodeClaimName).NotTo(BeEmpty(), "no NodeClaim found for providerID %s", providerID)

	GinkgoWriter.Printf("[gc] force-deleting NodeClaim %s to orphan the VM\n", nodeClaimName)

	// Remove the NodePool and Deployment first so Karpenter does not reprovision.
	env.DeleteDeployment(ctx, deployName)
	env.DeleteNodePool(ctx, nodePoolName)

	// Force-delete the NodeClaim: remove finalizer then delete.
	// This leaves the GCE VM running without any NodeClaim owner.
	env.ForceDeleteNodeClaim(ctx, nodeClaimName)

	GinkgoWriter.Printf("[gc] NodeClaim deleted; waiting for GC controller to delete orphaned VM...\n")

	// Wait for the GC controller to delete the orphaned VM.
	Expect(env.WaitForVMDeletion(ctx, providerID)).To(Succeed(),
		"GC controller did not delete orphaned VM %s within timeout", providerID)

	GinkgoWriter.Printf("[gc] VM deleted by GC controller; waiting for node %s to be removed...\n", provisionedNodeName)

	Expect(env.WaitForNodeRemoval(ctx, provisionedNodeName)).To(Succeed())
	provisionedNodeName = ""

	GinkgoWriter.Printf("[gc] node removed — GC controller test passed\n")
}

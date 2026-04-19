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

package repair_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

// nodeRepairTimeout covers the KernelDeadlock toleration window (5m) plus
// controller reconciliation lag and new node provisioning time. 15m gives
// headroom while keeping SpecTimeout at 25m — within the 30m go test budget.
const nodeRepairTimeout = 15 * time.Minute

var _ = Describe("NodeRepair", func() {
	It("should replace a node whose KernelDeadlock condition has been True beyond the toleration",
		func(ctx SpecContext) {
			runRepairTest(ctx, environment.TestCase{
				CapacityType:        karpv1.CapacityTypeOnDemand,
				Arch:                karpv1.ArchitectureAmd64,
				Families:            []string{"n2"},
				ConsolidationPolicy: "WhenEmpty",
			})
		}, SpecTimeout(nodeRepairTimeout+environment.ProvisioningTimeout))
})

func runRepairTest(ctx context.Context, tc environment.TestCase) {
	prefix := environment.TestPrefix(tc.Arch, tc.CapacityType, "repair")
	suffix := environment.UniqueSuffix()
	name := prefix + "-" + suffix

	var originalNodeName string
	DeferCleanup(func(ctx context.Context) {
		env.DeleteDeployment(ctx, name)
		env.DeleteNodePool(ctx, name)
		env.DeleteNodeClass(ctx, name)
		if originalNodeName != "" {
			_ = env.WaitForNodeRemoval(ctx, originalNodeName)
		}
	})

	env.CreateNodeClass(ctx, name)
	env.CreateNodePool(ctx, name, name, tc)
	env.CreateDeployment(ctx, name, name, name, tc.Arch)

	firstPod := env.WaitForRunningPod(ctx, name)
	Expect(firstPod.Spec.NodeName).NotTo(BeEmpty())
	originalNodeName = firstPod.Spec.NodeName

	// KernelDeadlock toleration is 5 minutes; the node.health controller then
	// deletes the node and Karpenter provisions a replacement.
	GinkgoWriter.Printf("patching KernelDeadlock=True on node %s\n", originalNodeName)
	patchNodeCondition(ctx, env.KubeClient, originalNodeName,
		"KernelDeadlock", corev1.ConditionTrue,
		"TestSimulation", "e2e repair test: simulated kernel deadlock")

	replacementPod := env.WaitForPodOnDifferentNode(ctx, name, originalNodeName, nodeRepairTimeout)
	Expect(replacementPod.Spec.NodeName).NotTo(Equal(originalNodeName),
		"pod must move to a replacement node after KernelDeadlock repair")

	Expect(env.WaitForNodeRemoval(ctx, originalNodeName)).To(Succeed(),
		"original node %s must be deleted after KernelDeadlock repair", originalNodeName)
	originalNodeName = ""
}

// patchNodeCondition sets or upserts a node status condition to simulate an
// unhealthy state without inducing actual OS failures on the GCE VM.
// LastTransitionTime is set to now so the repair toleration starts immediately.
// Retries on resourceVersion conflict — the kubelet updates node status every 10s.
// NPD-owned conditions (KernelDeadlock, ReadonlyFilesystem, etc.) are not reset by
// the kubelet between heartbeats, so the patched value remains stable until NPD clears it.
func patchNodeCondition(ctx context.Context, client kubernetes.Interface, nodeName string,
	condType corev1.NodeConditionType, status corev1.ConditionStatus,
	reason, message string) {
	err := wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		now := metav1.Now()
		newCond := corev1.NodeCondition{
			Type:               condType,
			Status:             status,
			LastHeartbeatTime:  now,
			LastTransitionTime: now,
			Reason:             reason,
			Message:            message,
		}
		found := false
		for i, c := range node.Status.Conditions {
			if c.Type == condType {
				node.Status.Conditions[i] = newCond
				found = true
				break
			}
		}
		if !found {
			node.Status.Conditions = append(node.Status.Conditions, newCond)
		}
		_, err = client.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			return false, nil
		}
		return err == nil, err
	})
	Expect(err).NotTo(HaveOccurred(), "patchNodeCondition: %s", nodeName)
}

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

	// Record the transition time once. GKE's embedded node monitoring periodically
	// resets NPD-owned conditions (KernelDeadlock, etc.) to False. To prevent the
	// node.health toleration clock from being reset each time GKE overwrites the
	// condition, all re-patches must reuse the same transition time so that
	// LastTransitionTime remains stable from the controller's perspective.
	transitionTime := metav1.Now()

	GinkgoWriter.Printf("patching KernelDeadlock=True on node %s (transitionTime=%s)\n",
		originalNodeName, transitionTime.Format(time.RFC3339))
	patchNodeCondition(ctx, env.KubeClient, originalNodeName,
		"KernelDeadlock", corev1.ConditionTrue, transitionTime,
		"TestSimulation", "e2e repair test: simulated kernel deadlock")

	// Hold the condition True against GKE's periodic resets. The goroutine
	// exits when the context is cancelled (spec completion) or the node is gone.
	holdNodeCondition(ctx, env.KubeClient, originalNodeName,
		"KernelDeadlock", corev1.ConditionTrue, transitionTime,
		"TestSimulation", "e2e repair test: simulated kernel deadlock")

	replacementPod := env.WaitForPodOnDifferentNode(ctx, name, originalNodeName, nodeRepairTimeout)
	Expect(replacementPod.Spec.NodeName).NotTo(Equal(originalNodeName),
		"pod must move to a replacement node after KernelDeadlock repair")

	Expect(env.WaitForNodeRemoval(ctx, originalNodeName)).To(Succeed(),
		"original node %s must be deleted after KernelDeadlock repair", originalNodeName)
	originalNodeName = ""
}

// holdNodeCondition launches a background goroutine that re-patches the given
// node condition every 30 s using the fixed transitionTime. GKE's embedded node
// monitoring periodically overwrites NPD-owned conditions to False; this keeps
// the condition stable so the node.health toleration clock runs from transitionTime.
// The goroutine exits when ctx is done or the node no longer exists.
// It is safe to call from a Ginkgo spec goroutine; no Gomega assertions are made
// inside the goroutine.
func holdNodeCondition(ctx context.Context, client kubernetes.Interface, nodeName string,
	condType corev1.NodeConditionType, status corev1.ConditionStatus,
	transitionTime metav1.Time, reason, message string) {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				err := applyNodeCondition(ctx, client, nodeName, condType, status, transitionTime, reason, message)
				if apierrors.IsNotFound(err) {
					return
				}
				if err != nil && ctx.Err() == nil {
					GinkgoWriter.Printf("holdNodeCondition: re-patch %s on %s: %v\n", condType, nodeName, err)
				}
			}
		}
	}()
}

// patchNodeCondition sets or upserts a node status condition, asserting success.
// transitionTime must be supplied by the caller; reuse the same value for any
// subsequent holdNodeCondition calls so the node.health toleration clock is not reset.
func patchNodeCondition(ctx context.Context, client kubernetes.Interface, nodeName string,
	condType corev1.NodeConditionType, status corev1.ConditionStatus,
	transitionTime metav1.Time, reason, message string) {
	err := applyNodeCondition(ctx, client, nodeName, condType, status, transitionTime, reason, message)
	Expect(err).NotTo(HaveOccurred(), "patchNodeCondition: %s", nodeName)
}

// applyNodeCondition performs a single attempt to upsert the node status condition,
// retrying on resourceVersion conflicts (the kubelet updates node status every 10 s).
func applyNodeCondition(ctx context.Context, client kubernetes.Interface, nodeName string,
	condType corev1.NodeConditionType, status corev1.ConditionStatus,
	transitionTime metav1.Time, reason, message string) error {
	return wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		newCond := corev1.NodeCondition{
			Type:               condType,
			Status:             status,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: transitionTime,
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
}

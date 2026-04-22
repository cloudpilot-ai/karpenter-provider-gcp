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

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

// nodeRepairTimeout is the budget for node.health to detect the back-dated
// KernelDeadlock condition and delete the NodeClaim. WaitForPodOnDifferentNode
// then has the remaining ProvisioningTimeout to boot the replacement.
const nodeRepairTimeout = 3 * time.Minute

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

	env.CreateNodeClass(ctx, name, gcpv1alpha1.ImageFamilyContainerOptimizedOS)
	env.CreateNodePool(ctx, name, name, tc)
	env.CreateDeployment(ctx, name, name, name, tc.Arch)

	firstPod := env.WaitForRunningPod(ctx, name)
	Expect(firstPod.Spec.NodeName).NotTo(BeEmpty())
	originalNodeName = firstPod.Spec.NodeName

	// Back-date by 6 min (1 min past the 5-min KernelDeadlock toleration) so
	// node.health fires in ~30 s instead of waiting 5 real minutes.
	// The hold goroutine reuses this fixed time so GKE can't reset the clock.
	transitionTime := metav1.NewTime(time.Now().Add(-6 * time.Minute))

	GinkgoWriter.Printf("patching KernelDeadlock=True on node %s (transitionTime=%s)\n",
		originalNodeName, transitionTime.Format(time.RFC3339))
	patchNodeCondition(ctx, env.KubeClient, originalNodeName,
		"KernelDeadlock", corev1.ConditionTrue, transitionTime,
		"TestSimulation", "e2e repair test: simulated kernel deadlock")

	// Hold the condition True against GKE's periodic resets. The goroutine
	// exits when the context is canceled (spec completion) or the node is gone.
	holdNodeCondition(ctx, env.KubeClient, originalNodeName,
		"KernelDeadlock", corev1.ConditionTrue, transitionTime,
		"TestSimulation", "e2e repair test: simulated kernel deadlock")

	replacementPod := env.WaitForPodOnDifferentNode(ctx, name, originalNodeName, environment.ProvisioningTimeout)
	Expect(replacementPod.Spec.NodeName).NotTo(Equal(originalNodeName),
		"pod must move to a replacement node after KernelDeadlock repair")

	Expect(env.WaitForNodeRemoval(ctx, originalNodeName)).To(Succeed(),
		"original node %s must be deleted after KernelDeadlock repair", originalNodeName)
	originalNodeName = ""
}

// holdNodeCondition re-patches the condition every 30 s with a fixed transitionTime.
// GKE's embedded node monitoring periodically resets NPD conditions to False; this
// keeps the condition alive until ctx is canceled or the node is gone.
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

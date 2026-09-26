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

package interruption

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

const nodeConditionTypeGCESpotPreempting = "GCESpotPreempting"

var env *environment.Environment
var _ = BeforeEach(func() { env = environment.Current() })

var _ = Describe("Interruption", Label("suite:interruption"), func() {
	It("should replace a Spot node reporting GCESpotPreempting=True", func(ctx SpecContext) {
		name := environment.TestPrefix(karpv1.ArchitectureAmd64, karpv1.CapacityTypeSpot, "interruption") + "-" + environment.UniqueSuffix()

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
		env.CreateNodePool(ctx, name, name, environment.TestCase{
			CapacityType: karpv1.CapacityTypeSpot,
			Arch:         karpv1.ArchitectureAmd64,
			Families:     []string{"n2"},
		})
		env.CreateDeployment(ctx, name, name, name, karpv1.ArchitectureAmd64)

		firstPod := env.WaitForRunningPod(ctx, name)
		originalNodeName = firstPod.Spec.NodeName
		originalNode, err := env.KubeClient.CoreV1().Nodes().Get(ctx, originalNodeName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(originalNode.Spec.ProviderID).NotTo(BeEmpty())

		GinkgoWriter.Printf("[interruption] setting %s=True on Spot node %s\n", nodeConditionTypeGCESpotPreempting, originalNodeName)
		setNodeCondition(ctx, originalNodeName, nodeConditionTypeGCESpotPreempting, corev1.ConditionTrue)

		replacementPod := env.WaitForPodOnDifferentNode(ctx, name, originalNodeName, environment.ReplacementTimeout)
		Expect(replacementPod.Spec.NodeName).NotTo(Equal(originalNodeName))

		Expect(env.WaitForNodeRemoval(ctx, originalNodeName)).To(Succeed())
		originalNodeName = ""
		Expect(env.WaitForVMDeletion(ctx, originalNode.Spec.ProviderID)).To(Succeed())
	}, SpecTimeout(environment.ReplacementTimeout+environment.NodeCleanupTimeout))
})

func setNodeCondition(ctx context.Context, nodeName string, conditionType corev1.NodeConditionType, status corev1.ConditionStatus) {
	err := wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		condition := corev1.NodeCondition{
			Type:               conditionType,
			Status:             status,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "TestSimulation",
			Message:            "e2e interruption test: simulated Spot preemption",
		}
		found := false
		for i := range node.Status.Conditions {
			if node.Status.Conditions[i].Type == conditionType {
				node.Status.Conditions[i] = condition
				found = true
				break
			}
		}
		if !found {
			node.Status.Conditions = append(node.Status.Conditions, condition)
		}
		_, err = env.KubeClient.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			return false, nil
		}
		return err == nil, err
	})
	Expect(err).NotTo(HaveOccurred(), "setting %s=%s on node %s", conditionType, status, nodeName)
}

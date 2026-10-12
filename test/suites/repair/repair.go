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

package repair

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

// Includes the minimum node age, blocker observations and event deduplication.
const nodeRepairTimeout = 10 * time.Minute

var env *environment.Environment
var _ = BeforeEach(func() { env = environment.Current() })

var _ = Describe("NodeRepair", Label("suite:repair"), func() {
	BeforeEach(requireNodeRepairEnabled)
	It("should honor repair budgets and vetoes, then initialize a replacement before deleting the unhealthy node",
		func(ctx SpecContext) {
			runRepairTest(ctx, environment.TestCase{
				CapacityType:        karpv1.CapacityTypeOnDemand,
				Arch:                karpv1.ArchitectureAmd64,
				Families:            []string{"n2"},
				ConsolidationPolicy: "WhenEmpty",
				DisruptionBudgets:   []map[string]any{{"nodes": "0"}},
			})
		}, SpecTimeout(nodeRepairTimeout+environment.ProvisioningTimeout))
})

// Fail before provisioning when node repair is not enabled in the controller.
func requireNodeRepairEnabled() {
	dep, err := env.KubeClient.AppsV1().Deployments(environment.KarpenterNamespace).
		Get(context.Background(), environment.KarpenterDeployment, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred(), "failed to get karpenter deployment")
	for _, c := range dep.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			Expect(e.Name == "LEGACY_NODE_REPAIR" && e.Value == "true").To(BeFalse(),
				"this spec requires modern repair; redeploy with controller.settings.legacyNodeRepair=false")
		}
		for _, e := range c.Env {
			if e.Name == "FEATURE_GATES" {
				Expect(strings.Contains(e.Value, "NodeRepair=true")).To(BeTrue(),
					"NodeRepair=true must be set in FEATURE_GATES; got %q — redeploy with --set controller.featureGates.nodeRepair=true", e.Value)
				return
			}
		}
	}
	Fail("FEATURE_GATES env var not found in karpenter deployment — is the controller deployed?")
}

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

	var original unstructured.Unstructured
	Eventually(func(g Gomega) {
		claims := poolClaims(ctx, name)
		g.Expect(claims).To(HaveLen(1))
		claim := nodeClaim(&claims[0])
		g.Expect(claim.StatusConditions().Get(karpv1.ConditionTypeInitialized).IsTrue()).To(BeTrue())
		original = claims[0]
	}).WithTimeout(environment.NodeClaimLaunchTimeout).WithPolling(environment.DefaultPollInterval).Should(Succeed())

	By("waiting until the node is old enough for the KernelDeadlock policy")
	originalNode, err := env.KubeClient.CoreV1().Nodes().Get(ctx, originalNodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	// Core clamps a backdated condition to Node creation; a fresh node still needs
	// five minutes of age before either blocker observation exercises eligible repair.
	Eventually(func() bool {
		return !time.Now().Before(originalNode.CreationTimestamp.Add(5 * time.Minute))
	}).WithTimeout(5 * time.Minute).WithPolling(environment.DefaultPollInterval).Should(BeTrue())

	By("holding an eligible fault behind an all-reason zero budget")
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

	expectNoRepair(ctx, name, original.GetUID())

	By("observing the node veto before allowing Unhealthy disruptions")
	_, err = env.KubeClient.CoreV1().Nodes().Patch(ctx, originalNodeName, types.MergePatchType,
		[]byte(`{"metadata":{"annotations":{"karpenter.sh/do-not-repair":"true"}}}`), metav1.PatchOptions{})
	Expect(err).NotTo(HaveOccurred())
	node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, originalNodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	Eventually(func(g Gomega) {
		events, err := env.KubeClient.CoreV1().Events("").List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.uid=" + string(node.UID)})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(events.Items).To(ContainElement(And(
			HaveField("Reason", "DisruptionBlocked"),
			HaveField("Message", `repair is blocked through the "karpenter.sh/do-not-repair" annotation`),
		)))
	}).WithTimeout(150 * time.Second).WithPolling(environment.DefaultPollInterval).Should(Succeed())
	_, err = env.DynamicClient.Resource(schema.GroupVersionResource{Group: "karpenter.sh", Version: "v1", Resource: "nodepools"}).Patch(ctx, name, types.MergePatchType,
		[]byte(`{"spec":{"disruption":{"budgets":[{"nodes":"0","reasons":["Empty","Underutilized","Drifted"]},{"nodes":"100%","reasons":["Unhealthy"]}]}}}`), metav1.PatchOptions{})
	Expect(err).NotTo(HaveOccurred())
	expectNoRepair(ctx, name, original.GetUID())

	By("initializing the replacement before admitting original deletion")
	watchCtx, cancel := context.WithTimeout(ctx, environment.ProvisioningTimeout)
	defer cancel()
	snapshot, stream, err := env.ListWatchNodeClaims(watchCtx, name)
	Expect(err).NotTo(HaveOccurred())
	defer stream.Stop()
	Expect(snapshot.Items).To(HaveLen(1))
	Expect(snapshot.Items[0].GetUID()).To(Equal(original.GetUID()))
	Expect(snapshot.Items[0].GetDeletionTimestamp()).To(BeNil())
	Expect(nodeClaim(&snapshot.Items[0]).StatusConditions().Get(karpv1.ConditionTypeInitialized).IsTrue()).To(BeTrue())
	_, err = env.KubeClient.CoreV1().Nodes().Patch(ctx, originalNodeName, types.MergePatchType,
		[]byte(`{"metadata":{"annotations":{"karpenter.sh/do-not-repair":null}}}`), metav1.PatchOptions{})
	Expect(err).NotTo(HaveOccurred())
	replacementNode, err := waitForReplacement(watchCtx, stream, original.GetUID())
	Expect(err).NotTo(HaveOccurred())
	replacementPod := env.WaitForPodOnDifferentNode(ctx, name, originalNodeName, environment.ProvisioningTimeout)
	Expect(replacementPod.Spec.NodeName).To(Equal(replacementNode),
		"workload must recover on the replacement observed by the watch")

	Expect(env.WaitForNodeRemoval(ctx, originalNodeName)).To(Succeed(),
		"original node %s must be deleted after KernelDeadlock repair", originalNodeName)
	originalNodeName = ""
}

func poolClaims(ctx context.Context, pool string) []unstructured.Unstructured {
	claims, err := env.ListNodeClaims(ctx)
	Expect(err).NotTo(HaveOccurred())
	var scoped []unstructured.Unstructured
	for _, claim := range claims {
		if claim.GetLabels()[karpv1.NodePoolLabelKey] == pool {
			scoped = append(scoped, claim)
		}
	}
	return scoped
}

func nodeClaim(object *unstructured.Unstructured) *karpv1.NodeClaim {
	claim := &karpv1.NodeClaim{}
	Expect(runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, claim)).To(Succeed())
	return claim
}

func expectNoRepair(ctx context.Context, pool string, uid types.UID) {
	// Several nominal 10-second disruption passes; also reject replacement admission,
	// since original survival alone would pass while a wrongly admitted replacement boots.
	Consistently(func(g Gomega) {
		claims := poolClaims(ctx, pool)
		g.Expect(claims).To(HaveLen(1))
		g.Expect(claims[0].GetUID()).To(Equal(uid))
		g.Expect(claims[0].GetDeletionTimestamp()).To(BeNil())
	}).WithTimeout(30 * time.Second).WithPolling(environment.DefaultPollInterval).Should(Succeed())
}

func waitForReplacement(ctx context.Context, stream watch.Interface, original types.UID) (string, error) {
	initialized := map[types.UID]string{}
	for {
		claim, deleting, err := nextNodeClaim(ctx, stream)
		if err != nil {
			return "", err
		}
		if claim.UID == original {
			if !deleting {
				continue
			}
			if len(initialized) != 1 {
				return "", fmt.Errorf("original deletion admitted before replacement initialization (active initialized replacements: %d)", len(initialized))
			}
			for _, nodeName := range initialized {
				return nodeName, nil
			}
		}
		delete(initialized, claim.UID)
		if !deleting && claim.StatusConditions().Get(karpv1.ConditionTypeInitialized).IsTrue() && claim.Status.NodeName != "" {
			initialized[claim.UID] = claim.Status.NodeName
		}
	}
}

func nextNodeClaim(ctx context.Context, stream watch.Interface) (*karpv1.NodeClaim, bool, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, false, fmt.Errorf("waiting for repair replacement/deletion ordering: %w", ctx.Err())
		case event, open := <-stream.ResultChan():
			if !open {
				return nil, false, fmt.Errorf("NodeClaim watch closed before repair ordering was established")
			}
			if event.Type == watch.Bookmark {
				continue
			}
			if event.Type == watch.Error {
				return nil, false, fmt.Errorf("NodeClaim watch failed: %w", apierrors.FromObject(event.Object))
			}
			object, ok := event.Object.(*unstructured.Unstructured)
			if !ok {
				return nil, false, fmt.Errorf("unexpected NodeClaim watch object %T", event.Object)
			}
			claim := &karpv1.NodeClaim{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, claim); err != nil {
				return nil, false, fmt.Errorf("decoding watched NodeClaim: %w", err)
			}
			return claim, event.Type == watch.Deleted || !claim.DeletionTimestamp.IsZero(), nil
		}
	}
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

/*
Copyright 2024 The CloudPilot AI Authors.

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

package provisioning_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

const testNamespace = "karpenter-e2e-test"

type provisioningCase struct {
	capacityType  string
	arch          string
	families      []string
	instanceTypes []string
}

// runProvisioningTest creates a NodeClass, NodePool, and Deployment for the
// given case, waits for a pod to run on a newly provisioned node, asserts the
// expected labels, then deletes the Deployment and waits for the node to be
// removed (confirming the GCP VM was also terminated).
func runProvisioningTest(ctx context.Context, tc provisioningCase) {
	suffix := uniqueSuffix()
	nodeClassName := "nodeclass-" + suffix
	nodePoolName := "nodepool-" + suffix
	deployName := "deploy-" + suffix
	appLabel := "app-" + suffix

	initialNodes := allNodeNames(ctx)

	var provisionedNodeName string
	DeferCleanup(func(ctx context.Context) {
		deleteDeployment(ctx, deployName)
		deleteNodePool(ctx, nodePoolName)
		deleteNodeClass(ctx, nodeClassName)
		if provisionedNodeName != "" {
			Expect(env.WaitForNodeRemoval(ctx, provisionedNodeName)).To(Succeed())
		}
	})

	createNodeClass(ctx, nodeClassName)
	createNodePool(ctx, nodePoolName, nodeClassName, tc)
	createDeployment(ctx, deployName, appLabel, nodePoolName)

	pod := waitForRunningPod(ctx, appLabel)
	Expect(pod.Spec.NodeName).NotTo(BeEmpty())

	node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	provisionedNodeName = node.Name

	nodeReady := false
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
			nodeReady = true
			break
		}
	}

	_, existedBefore := initialNodes[node.Name]
	Expect(existedBefore).To(BeFalse(), "expected a newly provisioned node, got a pre-existing one")
	Expect(nodeReady).To(BeTrue(), "node %s is not Ready", node.Name)
	Expect(node.Labels[karpv1.NodeRegisteredLabelKey]).To(Equal("true"))
	Expect(node.Labels[karpv1.NodePoolLabelKey]).To(Equal(nodePoolName))
	Expect(node.Labels[karpv1.CapacityTypeLabelKey]).To(Equal(tc.capacityType))
	Expect(node.Labels[corev1.LabelArchStable]).To(Equal(tc.arch))
	Expect(tc.families).To(ContainElement(node.Labels[gcpv1alpha1.LabelInstanceFamily]))
	Expect(tc.instanceTypes).To(ContainElement(node.Labels[corev1.LabelInstanceTypeStable]))
}

func createNodeClass(ctx context.Context, name string) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "karpenter.k8s.gcp/v1alpha1",
		"kind":       "GCENodeClass",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"imageSelectorTerms": []any{
				map[string]any{"alias": "ContainerOptimizedOS@latest"},
			},
			"disks": []any{
				map[string]any{"category": "pd-balanced", "sizeGiB": int64(30), "boot": true},
			},
			"subnetRangeName": env.PodsRangeName,
		},
	}}
	_, err := env.DynamicClient.Resource(environment.GCENodeClassGVR).Create(ctx, obj, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred(), "creating GCENodeClass %s", name)
}

func createNodePool(ctx context.Context, name, nodeClassName string, tc provisioningCase) {
	requirements := []any{
		map[string]any{"key": karpv1.CapacityTypeLabelKey, "operator": "In", "values": []any{tc.capacityType}},
		map[string]any{"key": gcpv1alpha1.LabelInstanceFamily, "operator": "In", "values": toAny(tc.families)},
		map[string]any{"key": corev1.LabelInstanceTypeStable, "operator": "In", "values": toAny(tc.instanceTypes)},
		map[string]any{"key": corev1.LabelArchStable, "operator": "In", "values": []any{tc.arch}},
		map[string]any{"key": corev1.LabelTopologyZone, "operator": "In", "values": []any{env.ClusterLocation}},
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "karpenter.sh/v1",
		"kind":       "NodePool",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"weight": int64(10),
			"disruption": map[string]any{
				"consolidateAfter":    "30s",
				"consolidationPolicy": "WhenEmptyOrUnderutilized",
				"budgets":             []any{map[string]any{"nodes": "100%"}},
			},
			"template": map[string]any{
				"spec": map[string]any{
					"nodeClassRef": map[string]any{
						"name":  nodeClassName,
						"kind":  "GCENodeClass",
						"group": "karpenter.k8s.gcp",
					},
					"requirements": requirements,
				},
			},
		},
	}}
	_, err := env.DynamicClient.Resource(environment.NodePoolGVR).Create(ctx, obj, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred(), "creating NodePool %s", name)
}

func createDeployment(ctx context.Context, name, appLabel, nodePoolName string) {
	replicas := int32(1)
	zero := int64(0)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": appLabel}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": appLabel}},
				Spec: corev1.PodSpec{
					NodeSelector:                  map[string]string{karpv1.NodePoolLabelKey: nodePoolName},
					TerminationGracePeriodSeconds: &zero,
					Containers: []corev1.Container{{
						Name:  "inflate",
						Image: environment.PauseImage,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
						},
					}},
				},
			},
		},
	}
	_, err := env.KubeClient.AppsV1().Deployments(testNamespace).Create(ctx, dep, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred(), "creating Deployment %s", name)
}

func waitForRunningPod(ctx context.Context, appLabel string) *corev1.Pod {
	var found *corev1.Pod
	Eventually(func(g Gomega) {
		found = nil // reset each poll so a pod that went non-Ready doesn't linger
		pods, err := env.KubeClient.CoreV1().Pods(testNamespace).List(ctx,
			metav1.ListOptions{LabelSelector: fmt.Sprintf("app=%s", appLabel)})
		g.Expect(err).NotTo(HaveOccurred())
		for i := range pods.Items {
			p := &pods.Items[i]
			if p.Status.Phase != corev1.PodRunning || p.Spec.NodeName == "" {
				continue
			}
			for _, c := range p.Status.Conditions {
				if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
					found = p.DeepCopy()
					return
				}
			}
		}
		g.Expect(found).NotTo(BeNil(), "no running pod with label app=%s", appLabel)
	}).WithTimeout(environment.ProvisioningTimeout).WithPolling(5 * time.Second).Should(Succeed())
	return found
}

func deleteDeployment(ctx context.Context, name string) {
	err := env.KubeClient.AppsV1().Deployments(testNamespace).Delete(ctx, name, metav1.DeleteOptions{})
	if !apierrors.IsNotFound(err) {
		Expect(err).NotTo(HaveOccurred(), "deleting Deployment %s", name)
	}
}

func deleteNodePool(ctx context.Context, name string) {
	err := env.DynamicClient.Resource(environment.NodePoolGVR).Delete(ctx, name, metav1.DeleteOptions{})
	if !apierrors.IsNotFound(err) {
		Expect(err).NotTo(HaveOccurred(), "deleting NodePool %s", name)
	}
}

func deleteNodeClass(ctx context.Context, name string) {
	err := env.DynamicClient.Resource(environment.GCENodeClassGVR).Delete(ctx, name, metav1.DeleteOptions{})
	if !apierrors.IsNotFound(err) {
		Expect(err).NotTo(HaveOccurred(), "deleting GCENodeClass %s", name)
	}
}

func allNodeNames(ctx context.Context) map[string]struct{} {
	nodes, err := env.KubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())
	result := make(map[string]struct{}, len(nodes.Items))
	for _, n := range nodes.Items {
		result[n.Name] = struct{}{}
	}
	return result
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// uniqueSuffix returns a short base-36 timestamp suffix, unique within a run.
func uniqueSuffix() string {
	raw := strconv.FormatInt(time.Now().UnixNano(), 36)
	// keep last 8 chars to stay within k8s name limits
	if len(raw) > 8 {
		raw = raw[len(raw)-8:]
	}
	return strings.ToLower(raw)
}

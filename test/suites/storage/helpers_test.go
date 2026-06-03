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

package storage_test

import (
	"context"
	"fmt"
	"sort"
	"time"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

func expectDiskTypeLabelsAndTopology(ctx context.Context, nodeName string, expected []string) {
	node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())

	for _, label := range expected {
		Expect(node.Labels).To(HaveKeyWithValue(label, "true"), "expected Node %s to have %s=true", nodeName, label)
	}

	Eventually(func(g Gomega) []string {
		csiNode, err := env.KubeClient.StorageV1().CSINodes().Get(ctx, nodeName, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		return topologyKeysForDriver(csiNode, "pd.csi.storage.gke.io")
	}, "2m", "5s").Should(ContainElements(expected), "expected PDCSI topology keys for Node %s", nodeName)
}

func createDeploymentWithNodeSelector(ctx context.Context, name, appLabel, nodePoolName string, extraNodeSelector map[string]string) {
	replicas := int32(1)
	zero := int64(0)
	nodeSelector := map[string]string{karpv1.NodePoolLabelKey: nodePoolName}
	for key, value := range extraNodeSelector {
		nodeSelector[key] = value
	}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: environment.TestNamespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": appLabel}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": appLabel}},
				Spec: corev1.PodSpec{
					NodeSelector:                  nodeSelector,
					TerminationGracePeriodSeconds: &zero,
					Tolerations: []corev1.Toleration{{
						Key:      "karpenter-e2e/nodepool",
						Value:    nodePoolName,
						Effect:   corev1.TaintEffectNoSchedule,
						Operator: corev1.TolerationOpEqual,
					}},
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

	_ = env.KubeClient.AppsV1().Deployments(environment.TestNamespace).Delete(ctx, name, metav1.DeleteOptions{})
	Eventually(func(g Gomega) {
		_, err := env.KubeClient.AppsV1().Deployments(environment.TestNamespace).Get(ctx, name, metav1.GetOptions{})
		g.Expect(errors.IsNotFound(err)).To(BeTrue(), "waiting for stale Deployment %s to be deleted", name)
	}).WithTimeout(environment.NodeCleanupTimeout).WithPolling(environment.DefaultPollInterval).Should(Succeed())

	_, err := env.KubeClient.AppsV1().Deployments(environment.TestNamespace).Create(ctx, dep, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred(), "creating Deployment %s", name)
}

func expectNoNodeClaimForNodePool(ctx context.Context, nodePoolName string, duration time.Duration) {
	Consistently(func(g Gomega) int {
		claims, err := env.ListNodeClaims(ctx)
		g.Expect(err).NotTo(HaveOccurred(), "listing NodeClaims")
		count := 0
		for _, claim := range claims {
			if claim.GetLabels()[karpv1.NodePoolLabelKey] == nodePoolName {
				count++
			}
		}
		return count
	}, duration, environment.DefaultPollInterval).Should(Equal(0), fmt.Sprintf("expected no NodeClaims for NodePool %s", nodePoolName))
}

func topologyKeysForDriver(csiNode *storagev1.CSINode, driverName string) []string {
	for _, driver := range csiNode.Spec.Drivers {
		if driver.Name == driverName {
			keys := append([]string(nil), driver.TopologyKeys...)
			sort.Strings(keys)
			return keys
		}
	}
	return nil
}

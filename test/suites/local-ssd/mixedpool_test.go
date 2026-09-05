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

package localssd_test

import (
	"context"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

var _ = Describe("Mixed local-SSD pool", func() {
	It("pd-balanced pool serves exact SSD counts including explicit zero", func(ctx SpecContext) {
		pool := newLocalSSDPool(ctx, environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2", "n2d"},
			InstanceTypes: []string{"n2-standard-2", "n2d-standard-4", "n2d-standard-8"},
			LocalSSDMode:  gcpv1alpha1.LocalSSDModeRawBlock,
			ExtraRequirements: []map[string]any{{
				"key":      gcpv1alpha1.LabelInstanceLocalSsdCount,
				"operator": "In",
				"values":   []any{"0", "2", "4"},
			}},
		})
		big := runPodOnPool(ctx, pool, "big", "n2d-standard-8", "4")
		small := runPodOnPool(ctx, pool, "small", "n2d-standard-4", "2")
		nossd := runPodOnPool(ctx, pool, "nossd", "n2-standard-2", "0")

		expectDistinctNodes(big, small, nossd)
		expectNodeShape(ctx, big, "n2d-standard-8", 4, "4")
		expectNodeShape(ctx, small, "n2d-standard-4", 2, "2")
		expectNodeShape(ctx, nossd, "n2-standard-2", 0, "0")
	}, SpecTimeout(20*time.Minute))

	It("hyperdisk-balanced pool serves bundled SKUs and a no-SSD pod", func(ctx SpecContext) {
		if os.Getenv("E2E_Z3_TESTS") != "true" {
			Skip("set E2E_Z3_TESTS=true to run z3 capacity-constrained tests")
		}
		pool := newLocalSSDPool(ctx, environment.TestCase{
			CapacityType:     karpv1.CapacityTypeOnDemand,
			Arch:             karpv1.ArchitectureAmd64,
			Families:         []string{"c4", "c4d", "z3"},
			InstanceTypes:    []string{"c4-standard-2", "c4d-standard-8-lssd", "z3-highmem-14-standardlssd"},
			BootDiskCategory: "hyperdisk-balanced",
			LocalSSDMode:     gcpv1alpha1.LocalSSDModeEphemeral,
		})
		z3 := runPodOnPool(ctx, pool, "z3", "z3-highmem-14-standardlssd", "")
		c4d := runPodOnPool(ctx, pool, "c4d", "c4d-standard-8-lssd", "")
		nossd := runPodOnPool(ctx, pool, "nossd", "c4-standard-2", "")

		expectDistinctNodes(z3, c4d, nossd)
		expectNodeShape(ctx, z3, "z3-highmem-14-standardlssd", 1, "1")
		expectNodeShape(ctx, c4d, "c4d-standard-8-lssd", 1, "1")
		expectNodeShape(ctx, nossd, "c4-standard-2", 0, "0")
	}, SpecTimeout(20*time.Minute))
})

func runPodOnPool(ctx context.Context, pool, suffix, instanceType, ssdCount string) *corev1.Node {
	app := pool + "-" + suffix
	selectors := map[string]string{corev1.LabelInstanceTypeStable: instanceType}
	if ssdCount != "" {
		selectors[gcpv1alpha1.LabelInstanceLocalSsdCount] = ssdCount
	}
	env.CreateDeploymentWithOptions(ctx, app, app, pool, karpv1.ArchitectureAmd64,
		environment.DeploymentOptions{ExtraNodeSelectors: selectors})
	pod := env.WaitForRunningPod(ctx, app)
	Expect(pod.Spec.NodeName).NotTo(BeEmpty())
	node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func(ctx context.Context) {
		env.DeleteDeployment(ctx, app)
		Expect(env.WaitForNodeRemoval(ctx, node.Name)).To(Succeed())
	})
	return node
}

func expectNodeShape(ctx context.Context, node *corev1.Node, instanceType string, scratchDiskCount int, expectedLabel string) {
	Expect(node.Labels[corev1.LabelInstanceTypeStable]).To(Equal(instanceType),
		"node %s: instance type", node.Name)

	inst, err := env.GetGCEInstance(ctx, node.Spec.ProviderID)
	Expect(err).NotTo(HaveOccurred(), "fetching GCE instance for %s", node.Name)
	var got int
	for _, d := range inst.Disks {
		if d.Type == "SCRATCH" && d.Interface == "NVME" {
			got++
		}
	}
	Expect(got).To(Equal(scratchDiskCount),
		"node %s: expected %d local SSDs, got %d", node.Name, scratchDiskCount, got)

	Eventually(func(g Gomega) {
		n, err := env.KubeClient.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(n.Labels).To(HaveKeyWithValue(gcpv1alpha1.LabelInstanceLocalSsdCount, expectedLabel),
			"node %s: %s = %q, want %q",
			node.Name, gcpv1alpha1.LabelInstanceLocalSsdCount,
			n.Labels[gcpv1alpha1.LabelInstanceLocalSsdCount], expectedLabel)
	}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
}

func expectDistinctNodes(nodes ...*corev1.Node) {
	seen := make(map[string]struct{}, len(nodes))
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		seen[n.Name] = struct{}{}
		names = append(names, n.Name)
	}
	Expect(seen).To(HaveLen(len(nodes)),
		"expected %d distinct nodes, got: %v", len(nodes), names)
}

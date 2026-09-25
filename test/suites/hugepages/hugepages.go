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

package hugepages

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

const (
	hugepages2Mi corev1.ResourceName = corev1.ResourceHugePagesPrefix + "2Mi"
	hugepages1Gi corev1.ResourceName = corev1.ResourceHugePagesPrefix + "1Gi"
)

// hugepagesCase describes one e2e scenario: provision a node from a
// GCENodeClass with spec.linuxNodeConfig.hugepages, advertise the pages to the
// scheduling simulation with a NodeOverlay, and run a pod that requests them.
// capacity is the NodeOverlay capacity, the pod request, and the expected
// node capacity, so the three always agree.
type hugepagesCase struct {
	tc        environment.TestCase
	hugepages map[string]any
	capacity  corev1.ResourceList
}

var env *environment.Environment
var _ = BeforeEach(func() { env = environment.Current() })

var _ = DescribeTable("static hugepages allocated on provisioned nodes", Label("suite:hugepages"),
	func(ctx SpecContext, c hugepagesCase) {
		runHugepagesTest(ctx, c)
	},

	// n2-standard-4 has 16 GiB. GKE limits hugepages to 60% of memory
	// on machines with less than 30 GB, so 1 GiB of 2 MiB pages fits.
	Entry("COS: hugepageSize2m allocates hugepages-2Mi", hugepagesCase{
		tc: environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2"},
			InstanceTypes: []string{"n2-standard-4"},
			ImageFamily:   gcpv1alpha1.ImageFamilyContainerOptimizedOS,
		},
		hugepages: map[string]any{"hugepageSize2m": int64(512)},
		capacity:  corev1.ResourceList{hugepages2Mi: resource.MustParse("1Gi")},
	}, SpecTimeout(15*time.Minute)),

	Entry("Ubuntu: hugepageSize2m allocates hugepages-2Mi", hugepagesCase{
		tc: environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2"},
			InstanceTypes: []string{"n2-standard-4"},
			ImageFamily:   gcpv1alpha1.ImageFamilyUbuntu,
		},
		hugepages: map[string]any{"hugepageSize2m": int64(512)},
		capacity:  corev1.ResourceList{hugepages2Mi: resource.MustParse("1Gi")},
	}, SpecTimeout(15*time.Minute)),

	// GKE supports 1 GiB pages only on some machine families, such as
	// C3. N2 does not support them.
	Entry("COS: hugepageSize1g allocates hugepages-1Gi", hugepagesCase{
		tc: environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"c3"},
			InstanceTypes: []string{"c3-standard-4"},
			ImageFamily:   gcpv1alpha1.ImageFamilyContainerOptimizedOS,
		},
		hugepages: map[string]any{"hugepageSize1g": int64(2)},
		capacity:  corev1.ResourceList{hugepages1Gi: resource.MustParse("2Gi")},
	}, SpecTimeout(15*time.Minute)),

	Entry("COS: hugepageSize2m and hugepageSize1g allocate both sizes", hugepagesCase{
		tc: environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"c3"},
			InstanceTypes: []string{"c3-standard-4"},
			ImageFamily:   gcpv1alpha1.ImageFamilyContainerOptimizedOS,
		},
		hugepages: map[string]any{"hugepageSize2m": int64(512), "hugepageSize1g": int64(2)},
		capacity: corev1.ResourceList{
			hugepages2Mi: resource.MustParse("1Gi"),
			hugepages1Gi: resource.MustParse("2Gi"),
		},
	}, SpecTimeout(15*time.Minute)),
)

// Fail before provisioning when node overlays are not enabled in the
// controller. Without them, no NodePool can schedule a pod that requests
// hugepages.
func requireNodeOverlayEnabled(ctx context.Context) {
	dep, err := env.KubeClient.AppsV1().Deployments(environment.KarpenterNamespace).
		Get(ctx, environment.KarpenterDeployment, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred(), "failed to get karpenter deployment")
	for _, c := range dep.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == "FEATURE_GATES" {
				Expect(strings.Contains(e.Value, "NodeOverlay=true")).To(BeTrue(),
					"NodeOverlay=true must be set in FEATURE_GATES; got %q — redeploy with --set controller.featureGates.nodeOverlay=true", e.Value)
				return
			}
		}
	}
	Fail("FEATURE_GATES env var not found in karpenter deployment — is the controller deployed?")
}

// runHugepagesTest provisions a node with the given hugepages and checks that
// the node reports them as capacity. The pod requests the hugepages, so it
// runs only when the NodeOverlay advertised them to the scheduling simulation
// and the node allocated them at boot.
func runHugepagesTest(ctx context.Context, c hugepagesCase) {
	requireNodeOverlayEnabled(ctx)

	prefix := environment.TestPrefix(c.tc.Arch, c.tc.CapacityType, osSlug(c.tc.ImageFamily), "hugepages")
	suffix := environment.UniqueSuffix()
	name := prefix + "-" + suffix

	GinkgoWriter.Printf("[setup] arch=%s capacityType=%s os=%s nodePool=%s hugepages=%v\n",
		c.tc.Arch, c.tc.CapacityType, c.tc.ImageFamily, name, c.hugepages)

	initialNodes := env.AllNodeNames(ctx)

	var provisionedNodeName string
	DeferCleanup(func(ctx context.Context) {
		env.DeleteDeployment(ctx, name)
		env.DeleteNodePool(ctx, name)
		env.DeleteNodeOverlay(ctx, name)
		env.DeleteNodeClass(ctx, name)
		if provisionedNodeName != "" {
			Expect(env.WaitForNodeRemoval(ctx, provisionedNodeName)).To(Succeed())
		}
	})

	env.CreateNodeClassWithHugepages(ctx, name, c.tc.ImageFamily, c.hugepages)
	env.WaitForNodeClassReady(ctx, name)
	env.CreateNodePool(ctx, name, name, c.tc)
	env.WaitForNodePoolReady(ctx, name)
	env.CreateNodeOverlay(ctx, name, name, c.capacity)
	env.WaitForNodeOverlayReady(ctx, name)
	env.CreateDeploymentWithHugepages(ctx, name, name, name, c.tc.Arch, c.capacity)

	env.WaitForNodeClaimLaunched(ctx, name)
	pod := env.WaitForRunningPod(ctx, name)
	Expect(pod.Spec.NodeName).NotTo(BeEmpty())

	node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	provisionedNodeName = node.Name

	_, existedBefore := initialNodes[node.Name]
	Expect(existedBefore).To(BeFalse(), "expected a newly provisioned node, got a pre-existing one")
	Expect(environment.IsNodeReady(node)).To(BeTrue(), "node %s is not Ready", node.Name)
	Expect(c.tc.InstanceTypes).To(ContainElement(node.Labels[corev1.LabelInstanceTypeStable]))

	for resourceName, want := range c.capacity {
		got := node.Status.Capacity[resourceName]
		Expect(got.Cmp(want)).To(Equal(0),
			"capacity %s %s must equal the GCENodeClass hugepages %s", resourceName, got.String(), want.String())
	}
}

func osSlug(imageFamily string) string {
	if imageFamily == gcpv1alpha1.ImageFamilyUbuntu {
		return "ubuntu"
	}
	return "cos"
}

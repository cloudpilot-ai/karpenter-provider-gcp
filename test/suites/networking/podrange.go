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

package networking

import (
	"context"
	"slices"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/api/compute/v1"
	containerv1 "google.golang.org/api/container/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

var _ = Describe("Pod ranges", Serial, Label("suite:networking"), func() {
	var cluster *containerv1.Cluster
	BeforeEach(func(ctx SpecContext) {
		Expect(env.SmallPodsRangeName).NotTo(BeEmpty(), "set SMALL_PODS_RANGE_NAME and run e2e-setup")
		Expect(env.LargePodsRangeName).NotTo(BeEmpty(), "set LARGE_PODS_RANGE_NAME and run e2e-setup")
		Expect(env.SmallPodsRangeName).NotTo(Equal(env.LargePodsRangeName))
		var err error
		cluster, err = env.GetCluster(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(cluster.IpAllocationPolicy).NotTo(BeNil())
		Expect(cluster.IpAllocationPolicy.ClusterSecondaryRangeName).To(Equal(env.PodsRangeName))
		Expect(cluster.IpAllocationPolicy.AdditionalPodRangesConfig).NotTo(BeNil(), "run e2e-setup to attach pod ranges")
		Expect(cluster.IpAllocationPolicy.AdditionalPodRangesConfig.PodRangeNames).To(ContainElements(env.SmallPodsRangeName, env.LargePodsRangeName))
	})

	It("discovers the default and GKE additional pod ranges without overrides", func(ctx SpecContext) {
		f := newPodRangeFixture(ctx, "", nil)
		expected := clusterPodRangeNames(cluster)
		waitForPodRangeNames(ctx, f.name, expected)
		instance := f.provision(ctx)
		Expect(primaryAliasRange(instance)).To(BeElementOf(expected))
	}, SpecTimeout(15*time.Minute))

	It("publishes Compute free-IP counts for additional ranges", func(ctx SpecContext) {
		f := newPodRangeFixture(ctx, "", []string{env.SmallPodsRangeName, env.LargePodsRangeName})
		waitForPodRangeFreeIPs(ctx, f.name, cluster, []string{env.SmallPodsRangeName, env.LargePodsRangeName})
	}, SpecTimeout(10*time.Minute))

	DescribeTable("explicit range overrides replace discovery", func(ctx SpecContext, useSubnetRangeName bool) {
		var value string
		var names []string
		if useSubnetRangeName {
			value = env.SmallPodsRangeName
		} else {
			names = []string{env.SmallPodsRangeName}
		}
		f := newPodRangeFixture(ctx, value, names)
		waitForPodRangeNames(ctx, f.name, []string{env.SmallPodsRangeName})
		Expect(primaryAliasRange(f.provision(ctx))).To(Equal(env.SmallPodsRangeName))
	},
		Entry("subnetRangeName", true, SpecTimeout(15*time.Minute)),
		Entry("subnetRangeNames with one range", false, SpecTimeout(15*time.Minute)),
	)

	It("selects the richer range even when the poorer range is listed first", func(ctx SpecContext) {
		names := []string{env.SmallPodsRangeName, env.LargePodsRangeName}
		f := newPodRangeFixture(ctx, "", names)
		waitForPodRangeNames(ctx, f.name, names)
		var counts map[string]*computepb.SubnetworkUtilizationDetailsIPV4Utilization
		Eventually(func(g Gomega) {
			counts = readPodRangeUtilization(ctx, cluster)
			for _, name := range names {
				g.Expect(counts).To(HaveKey(name))
				g.Expect(counts[name].TotalFreeIp).NotTo(BeNil())
				// Both candidates must fit a node CIDR so exhaustion fallback cannot hide broken ranking.
				g.Expect(counts[name].GetTotalFreeIp()).To(BeNumerically(">=", 1024), "range %s must have room for a node CIDR", name)
			}
			g.Expect(counts[env.LargePodsRangeName].GetTotalFreeIp()).To(BeNumerically(">", counts[env.SmallPodsRangeName].GetTotalFreeIp()))
		}).WithContext(ctx).WithTimeout(3 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
		GinkgoWriter.Printf("[ranking] poorer=%s freeIP=%d richer=%s freeIP=%d\n", env.SmallPodsRangeName, counts[env.SmallPodsRangeName].GetTotalFreeIp(), env.LargePodsRangeName, counts[env.LargePodsRangeName].GetTotalFreeIp())
		Expect(primaryAliasRange(f.provision(ctx))).To(Equal(env.LargePodsRangeName))
	}, SpecTimeout(15*time.Minute))
})

type podRangeFixture struct {
	name     string
	nodeName string
}

func newPodRangeFixture(ctx context.Context, subnetRangeName string, names []string) *podRangeFixture {
	f := &podRangeFixture{name: "pod-ranges-" + environment.UniqueSuffix()}
	DeferCleanup(func(ctx SpecContext) {
		env.DeleteDeployment(ctx, f.name)
		env.DeleteNodePool(ctx, f.name)
		env.DeleteNodeClass(ctx, f.name)
		if f.nodeName != "" {
			Expect(env.WaitForNodeRemoval(ctx, f.nodeName)).To(Succeed())
		}
	}, NodeTimeout(10*time.Minute))
	env.CreateNodeClassWithPodRanges(ctx, f.name, subnetRangeName, names)
	env.WaitForNodeClassReady(ctx, f.name)
	return f
}

func (f *podRangeFixture) provision(ctx context.Context) *compute.Instance {
	tc := environment.TestCase{
		CapacityType: karpv1.CapacityTypeOnDemand, Arch: karpv1.ArchitectureAmd64,
		InstanceTypes: []string{"n2-standard-2"}, ConsolidationPolicy: "WhenEmpty",
	}
	env.CreateNodePool(ctx, f.name, f.name, tc)
	env.WaitForNodePoolReady(ctx, f.name)
	env.CreateDeployment(ctx, f.name, f.name, f.name, tc.Arch)
	pod := env.WaitForRunningPod(ctx, f.name)
	f.nodeName = pod.Spec.NodeName
	node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, f.nodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	Expect(environment.IsNodeReady(node)).To(BeTrue())
	Expect(node.Labels).To(HaveKeyWithValue(karpv1.NodePoolLabelKey, f.name))
	claims, err := env.ListNodeClaims(ctx)
	Expect(err).NotTo(HaveOccurred())
	var owned int
	for _, claim := range claims {
		if claim.GetLabels()[karpv1.NodePoolLabelKey] == f.name {
			owned++
		}
	}
	Expect(owned).To(Equal(1), "expected exactly one NodeClaim for isolated workload")
	instance, err := env.GetGCEInstance(ctx, node.Spec.ProviderID)
	Expect(err).NotTo(HaveOccurred())
	return instance
}

func primaryAliasRange(instance *compute.Instance) string {
	Expect(instance.NetworkInterfaces).NotTo(BeEmpty())
	Expect(instance.NetworkInterfaces[0].AliasIpRanges).To(HaveLen(1))
	return instance.NetworkInterfaces[0].AliasIpRanges[0].SubnetworkRangeName
}

func clusterPodRangeNames(cluster *containerv1.Cluster) []string {
	policy := cluster.IpAllocationPolicy
	names := []string{policy.ClusterSecondaryRangeName}
	for _, name := range policy.AdditionalPodRangesConfig.PodRangeNames {
		if name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	for _, info := range policy.AdditionalPodRangesConfig.PodRangeInfo {
		if info != nil && info.RangeName != "" && !slices.Contains(names, info.RangeName) {
			names = append(names, info.RangeName)
		}
	}
	return names
}

func readPodRangeUtilization(ctx context.Context, cluster *containerv1.Cluster) map[string]*computepb.SubnetworkUtilizationDetailsIPV4Utilization {
	lookupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	subnet, err := env.GetClusterSubnetwork(lookupCtx, cluster)
	Expect(err).NotTo(HaveOccurred())
	counts := map[string]*computepb.SubnetworkUtilizationDetailsIPV4Utilization{}
	for _, entry := range subnet.GetUtilizationDetails().GetIpv4Utilizations() {
		if entry.GetRangeName() != "" {
			counts[entry.GetRangeName()] = entry
		}
	}
	return counts
}

func waitForPodRangeNames(ctx context.Context, name string, expected []string) {
	Eventually(func(g Gomega) {
		nodeClass, err := env.GetNodeClass(ctx, name)
		g.Expect(err).NotTo(HaveOccurred())
		var names []string
		for _, entry := range nodeClass.Status.SubnetRanges {
			names = append(names, entry.Name)
		}
		g.Expect(names).To(ConsistOf(expected))
	}).WithContext(ctx).WithTimeout(time.Minute).WithPolling(time.Second).Should(Succeed())
}

// Allow the five-minute status refresh if a previous spec's allocation is still cached.
func waitForPodRangeFreeIPs(ctx context.Context, name string, cluster *containerv1.Cluster, expected []string) {
	Eventually(func(g Gomega) {
		before := readPodRangeUtilization(ctx, cluster)
		nodeClass, err := env.GetNodeClass(ctx, name)
		g.Expect(err).NotTo(HaveOccurred())
		after := readPodRangeUtilization(ctx, cluster)
		var names []string
		for _, entry := range nodeClass.Status.SubnetRanges {
			names = append(names, entry.Name)
			g.Expect(before).To(HaveKey(entry.Name))
			g.Expect(after).To(HaveKey(entry.Name))
			g.Expect(before[entry.Name].TotalFreeIp).NotTo(BeNil())
			g.Expect(after[entry.Name].TotalFreeIp).NotTo(BeNil())
			g.Expect(after[entry.Name].GetTotalFreeIp()).To(Equal(before[entry.Name].GetTotalFreeIp()), "capacity changed while comparing status")
			g.Expect(entry.TotalFreeIP).NotTo(BeNil())
			g.Expect(*entry.TotalFreeIP).To(Equal(after[entry.Name].GetTotalFreeIp()), "status capacity for %s", entry.Name)
		}
		g.Expect(names).To(ConsistOf(expected))
	}).WithContext(ctx).WithTimeout(7 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())
}

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
	"net"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

var _ = Describe("Multi-network Pod CIDR", Serial, Label("suite:networking"), func() {
	DescribeTable("initializes a range not registered by a running GKE pool", func(ctx SpecContext, explicit bool) {
		cluster, err := env.GetCluster(ctx)
		Expect(err).NotTo(HaveOccurred())
		if cluster.NetworkConfig == nil || !cluster.NetworkConfig.EnableMultiNetworking {
			Skip("requires a multi-networking-enabled GKE cluster")
		}
		Expect(cluster.Autoscaling == nil || !cluster.Autoscaling.EnableNodeAutoprovisioning).To(BeTrue(), "disable NAP for the isolated inactive-range regression")
		network := managedNetworkObject(ctx, managedNetworkResource("networks"), "default")
		mode, _, err := unstructured.NestedString(network.Object, "spec", "IPAMMode")
		Expect(err).NotTo(HaveOccurred())
		Expect(mode).To(Equal("Internal"), "requires Internal default-network IPAM")
		parameters, _, err := unstructured.NestedString(network.Object, "spec", "parametersRef", "name")
		Expect(err).NotTo(HaveOccurred())
		Expect(parameters).NotTo(BeEmpty())
		gnp := managedNetworkResource("gkenetworkparamsets")
		Expect(cluster.IpAllocationPolicy).NotTo(BeNil())
		Expect(cluster.IpAllocationPolicy.AdditionalPodRangesConfig).NotTo(BeNil(), "configure an unused additional Pod range for this regression")

		var target string
		Eventually(func(g Gomega) {
			registered := registeredPodRanges(ctx, gnp, parameters)
			counts := readPodRangeUtilization(ctx, cluster)
			var richest int64 = -1
			target = ""
			for _, name := range clusterPodRangeNames(cluster) {
				g.Expect(counts).To(HaveKey(name))
				g.Expect(counts[name].TotalFreeIp).NotTo(BeNil())
				if explicit && slices.Contains(registered, name) {
					continue
				}
				if free := counts[name].GetTotalFreeIp(); free > richest {
					target, richest = name, free
				}
			}
			g.Expect(target).NotTo(BeEmpty(), "configure a Pod range absent from GNP; do not substitute an active range")
			g.Expect(richest).To(BeNumerically(">=", 1024), "unused range needs room for a node CIDR")
			g.Expect(registered).NotTo(ContainElement(target), "omitted-range regression requires the richest discovered range to be unused; provide a larger unused secondary range")
		}).WithContext(ctx).WithTimeout(3 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
		GinkgoWriter.Printf("[multi-network] explicit=%t target=%s initially absent from GNP/%s\n", explicit, target, parameters)

		var names []string
		if explicit {
			names = []string{target}
		}
		f := newPodRangeFixture(ctx, "", names)
		instance := f.provision(ctx)
		Expect(primaryAliasRange(instance)).To(Equal(target), "autodiscovery must exercise the unused range, not silently pass on an active one")
		node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, f.nodeName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		alias := instance.NetworkInterfaces[0].AliasIpRanges[0]
		Expect(node.Spec.PodCIDR).To(Equal(alias.IpCidrRange))
		Expect(node.Spec.PodCIDRs).To(ContainElement(alias.IpCidrRange))
		subnet := instance.NetworkInterfaces[0].Subnetwork
		Expect(node.Labels).To(HaveKeyWithValue("cloud.google.com/gke-np-default-subnet", subnet[strings.LastIndex(subnet, "/")+1:]))
		Expect(node.Labels).To(HaveKeyWithValue("cloud.google.com/gke-np-default-pod-range", target))
		// GKE may now add the labeled Node's range to GNP; absence is a launch precondition only.
		checkPodDNSAndService(ctx, f, node, alias.IpCidrRange)
	},
		Entry("explicit range", true, SpecTimeout(20*time.Minute)),
		Entry("omitted ranges use discovery", false, SpecTimeout(20*time.Minute)),
	)
})

func managedNetworkResource(resource string) schema.GroupVersionResource {
	groups, err := env.KubeClient.Discovery().ServerGroups()
	Expect(err).NotTo(HaveOccurred())
	for _, group := range groups.Groups {
		if group.Name != "networking.gke.io" {
			continue
		}
		for _, version := range append([]metav1.GroupVersionForDiscovery{group.PreferredVersion}, group.Versions...) {
			resources, err := env.KubeClient.Discovery().ServerResourcesForGroupVersion(version.GroupVersion)
			if err != nil {
				continue
			}
			for _, entry := range resources.APIResources {
				if entry.Name == resource {
					Expect(entry.Namespaced).To(BeFalse())
					return schema.GroupVersionResource{Group: group.Name, Version: version.Version, Resource: resource}
				}
			}
		}
	}
	Fail("multi-networking enabled but managed networking API is missing: " + resource)
	return schema.GroupVersionResource{}
}

func managedNetworkObject(ctx context.Context, resource schema.GroupVersionResource, name string) *unstructured.Unstructured {
	obj, err := env.DynamicClient.Resource(resource).Get(ctx, name, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	return obj
}

func registeredPodRanges(ctx context.Context, resource schema.GroupVersionResource, name string) []string {
	obj := managedNetworkObject(ctx, resource, name)
	ranges, found, err := unstructured.NestedStringSlice(obj.Object, "spec", "podIPv4Ranges", "rangeNames")
	Expect(err).NotTo(HaveOccurred())
	Expect(found).To(BeTrue())
	return ranges
}

func checkPodDNSAndService(ctx SpecContext, f *podRangeFixture, node *corev1.Node, cidr string) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: f.name + "-network", OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Node", Name: node.Name, UID: node.UID}}},
		Spec: corev1.PodSpec{
			NodeName: node.Name, RestartPolicy: corev1.RestartPolicyNever, AutomountServiceAccountToken: new(bool),
			Tolerations: []corev1.Toleration{{Key: "karpenter-e2e/nodepool", Operator: corev1.TolerationOpEqual, Value: f.name, Effect: corev1.TaintEffectNoSchedule}},
			Containers: []corev1.Container{{Name: "network", Image: "busybox:1.36.1", Command: []string{"sh", "-c", `set -eu
nslookup kubernetes.default.svc.cluster.local
if wget -T 10 --no-check-certificate -S -O /dev/null https://kubernetes.default.svc.cluster.local/version 2>/tmp/response; then
  cat /tmp/response
else
  cat /tmp/response
  grep -Eq 'HTTP/[0-9.]+ (401|403)' /tmp/response
fi`}}},
		},
	}
	created, err := env.KubeClient.CoreV1().Pods(environment.TestNamespace).Create(ctx, pod, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func(ctx SpecContext) {
		uid := created.UID
		err := env.KubeClient.CoreV1().Pods(environment.TestNamespace).Delete(ctx, created.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		if !apierrors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred())
		}
	})
	Eventually(func(g Gomega) {
		current, err := env.KubeClient.CoreV1().Pods(environment.TestNamespace).Get(ctx, created.Name, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(current.Status.Phase).To(Equal(corev1.PodSucceeded), "DNS/API ClusterIP probe must succeed")
		_, block, err := net.ParseCIDR(cidr)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(block.Contains(net.ParseIP(current.Status.PodIP))).To(BeTrue())
	}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(3 * time.Second).Should(Succeed())
}

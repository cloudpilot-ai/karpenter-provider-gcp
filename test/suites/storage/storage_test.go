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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

var _ = Describe("Default Disk Selection", func() {
	It("should use the machine-series default when disk category is omitted", func(ctx SpecContext) {
		prefix := environment.TestPrefix(karpv1.ArchitectureAmd64, karpv1.CapacityTypeOnDemand, "default-disk")
		nodeClassName := prefix + "-" + environment.UniqueSuffix()

		env.CreateNodeClassWithDefaultDisk(ctx, nodeClassName, gcpv1alpha1.ImageFamilyContainerOptimizedOS)
		env.WaitForNodeClassReady(ctx, nodeClassName)
		DeferCleanup(func(ctx context.Context) {
			env.DeleteNodeClass(ctx, nodeClassName)
		})

		cases := []struct {
			family       string
			instanceType string
			diskType     string
		}{
			{family: "n2", instanceType: "n2-standard-2", diskType: "pd-standard"},
			{family: "c3", instanceType: "c3-standard-4", diskType: "pd-balanced"},
			{family: "n4", instanceType: "n4-standard-2", diskType: "hyperdisk-balanced"},
		}
		for _, tc := range cases {
			By(fmt.Sprintf("provisioning %s with %s", tc.family, tc.diskType))
			name := nodeClassName + "-" + tc.family
			var provisionedNodeName string
			DeferCleanup(func(ctx context.Context) {
				env.DeleteDeployment(ctx, name)
				env.DeleteNodePool(ctx, name)
				if provisionedNodeName != "" {
					Expect(env.WaitForNodeRemoval(ctx, provisionedNodeName)).To(Succeed())
				}
			})

			env.CreateNodePool(ctx, name, nodeClassName, environment.TestCase{
				CapacityType:  karpv1.CapacityTypeOnDemand,
				Arch:          karpv1.ArchitectureAmd64,
				Families:      []string{tc.family},
				InstanceTypes: []string{tc.instanceType},
			})
			env.WaitForNodePoolReady(ctx, name)
			env.CreateDeployment(ctx, name, name, name, karpv1.ArchitectureAmd64)

			pod := env.WaitForRunningPod(ctx, name)
			Expect(pod.Spec.NodeName).NotTo(BeEmpty())
			provisionedNodeName = pod.Spec.NodeName
			node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, provisionedNodeName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(node.Spec.ProviderID).NotTo(BeEmpty())

			diskType, err := env.GetGCEBootDiskType(ctx, node.Spec.ProviderID)
			Expect(err).NotTo(HaveOccurred())
			Expect(diskType).To(Equal(tc.diskType))

			env.DeleteDeployment(ctx, name)
			env.DeleteNodePool(ctx, name)
			Expect(env.WaitForNodeRemoval(ctx, provisionedNodeName)).To(Succeed())
			provisionedNodeName = ""
		}
	}, SpecTimeout(25*time.Minute))
})

var _ = Describe("PDCSI Disk Type Labels", func() {
	It("should schedule a pod that requires a supported disk type label before the node exists", func(ctx SpecContext) {
		prefix := "amd64-od-disk-label-sched"
		suffix := environment.UniqueSuffix()
		name := prefix + "-" + suffix

		initialNodes := env.AllNodeNames(ctx)
		var provisionedNodeName string
		DeferCleanup(func(ctx context.Context) {
			env.DeleteDeployment(ctx, name)
			env.DeleteNodePool(ctx, name)
			env.DeleteNodeClass(ctx, name)
			if provisionedNodeName != "" {
				Expect(env.WaitForNodeRemoval(ctx, provisionedNodeName)).To(Succeed())
			}
		})

		env.CreateNodeClass(ctx, name, gcpv1alpha1.ImageFamilyContainerOptimizedOS)
		env.WaitForNodeClassReady(ctx, name)
		env.CreateNodePool(ctx, name, name, environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"e2"},
			InstanceTypes: []string{"e2-standard-4"},
		})
		env.WaitForNodePoolReady(ctx, name)
		createDeploymentWithNodeSelector(ctx, name, name, name, map[string]string{
			"disk-type.gke.io/pd-balanced": "true",
		})

		env.WaitForNodeClaimLaunched(ctx, name)
		pod := env.WaitForRunningPod(ctx, name)
		Expect(pod.Spec.NodeName).NotTo(BeEmpty())
		provisionedNodeName = pod.Spec.NodeName

		_, existedBefore := initialNodes[provisionedNodeName]
		Expect(existedBefore).To(BeFalse(), "expected a newly provisioned node, got a pre-existing one")
		expectDiskTypeLabelsAndTopology(ctx, provisionedNodeName, []string{"disk-type.gke.io/pd-balanced"})
	}, SpecTimeout(15*time.Minute))

	It("should not schedule a pod that requires an unsupported disk type label", func(ctx SpecContext) {
		prefix := "amd64-od-disk-label-no-sched"
		suffix := environment.UniqueSuffix()
		name := prefix + "-" + suffix

		DeferCleanup(func(ctx context.Context) {
			env.DeleteDeployment(ctx, name)
			env.DeleteNodePool(ctx, name)
			env.DeleteNodeClass(ctx, name)
		})

		env.CreateNodeClass(ctx, name, gcpv1alpha1.ImageFamilyContainerOptimizedOS)
		env.WaitForNodeClassReady(ctx, name)
		env.CreateNodePool(ctx, name, name, environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"e2"},
			InstanceTypes: []string{"e2-standard-4"},
		})
		env.WaitForNodePoolReady(ctx, name)
		createDeploymentWithNodeSelector(ctx, name, name, name, map[string]string{
			"disk-type.gke.io/hyperdisk-throughput": "true",
		})

		expectNoNodeClaimForNodePool(ctx, name, 2*time.Minute)
	}, SpecTimeout(5*time.Minute))

	It("should register disk type labels and topology for the selected machine family", func(ctx SpecContext) {
		prefix := "amd64-od-disk-labels"
		suffix := environment.UniqueSuffix()
		name := prefix + "-" + suffix

		initialNodes := env.AllNodeNames(ctx)
		var provisionedNodeName string
		DeferCleanup(func(ctx context.Context) {
			env.DeleteDeployment(ctx, name)
			env.DeleteNodePool(ctx, name)
			env.DeleteNodeClass(ctx, name)
			if provisionedNodeName != "" {
				Expect(env.WaitForNodeRemoval(ctx, provisionedNodeName)).To(Succeed())
			}
		})

		env.CreateNodeClass(ctx, name, gcpv1alpha1.ImageFamilyContainerOptimizedOS)
		env.WaitForNodeClassReady(ctx, name)
		env.CreateNodePool(ctx, name, name, environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"e2"},
			InstanceTypes: []string{"e2-standard-4"},
		})
		env.WaitForNodePoolReady(ctx, name)
		env.CreateDeployment(ctx, name, name, name, karpv1.ArchitectureAmd64)

		env.WaitForNodeClaimLaunched(ctx, name)
		pod := env.WaitForRunningPod(ctx, name)
		Expect(pod.Spec.NodeName).NotTo(BeEmpty())
		provisionedNodeName = pod.Spec.NodeName

		_, existedBefore := initialNodes[provisionedNodeName]
		Expect(existedBefore).To(BeFalse(), "expected a newly provisioned node, got a pre-existing one")

		expectDiskTypeLabelsAndTopology(ctx, provisionedNodeName, []string{
			"disk-type.gke.io/pd-balanced",
			"disk-type.gke.io/pd-extreme",
			"disk-type.gke.io/pd-ssd",
			"disk-type.gke.io/pd-standard",
		})
	}, SpecTimeout(15*time.Minute))
})

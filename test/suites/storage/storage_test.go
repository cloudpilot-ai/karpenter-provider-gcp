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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

var _ = Describe("PDCSI Disk Type Labels", func() {
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

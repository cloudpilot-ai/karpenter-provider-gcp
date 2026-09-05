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
	"time"

	. "github.com/onsi/ginkgo/v2"
	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

var _ = Describe("Local-SSD failure modes", func() {
	It("leaves the pod Pending when the pinned SSD-count mismatches a bundled SKU", func(ctx SpecContext) {
		name := newLocalSSDPool(ctx, environment.TestCase{
			CapacityType:     karpv1.CapacityTypeOnDemand,
			Arch:             karpv1.ArchitectureAmd64,
			Families:         []string{"z3"},
			InstanceTypes:    []string{"z3-highmem-22-standardlssd"},
			BootDiskCategory: "hyperdisk-balanced",
			LocalSSDMode:     gcpv1alpha1.LocalSSDModeEphemeral,
		})
		env.CreateDeploymentWithOptions(ctx, name, name, name, karpv1.ArchitectureAmd64,
			environment.DeploymentOptions{ExtraNodeSelectors: map[string]string{
				corev1.LabelInstanceTypeStable:         "z3-highmem-22-standardlssd",
				gcpv1alpha1.LabelInstanceLocalSsdCount: "4",
			}})
		DeferCleanup(func(ctx context.Context) { env.DeleteDeployment(ctx, name) })
		env.ConsistentlyExpectPendingPods(ctx, name, 30*time.Second)
		env.ExpectNoNodeClaim(ctx, name)
	}, SpecTimeout(10*time.Minute))
})

func newLocalSSDPool(ctx context.Context, tc environment.TestCase) string {
	prefix := environment.TestPrefix(tc.Arch, tc.CapacityType, "cos", "lssd")
	name := prefix + "-" + environment.UniqueSuffix()
	DeferCleanup(func(ctx context.Context) {
		env.DeleteNodePool(ctx, name)
		env.DeleteNodeClass(ctx, name)
	})
	env.CreateNodeClassForLocalSSD(ctx, name, gcpv1alpha1.ImageFamilyContainerOptimizedOS,
		tc.BootDiskCategory, tc.LocalSSDMode)
	env.WaitForNodeClassReady(ctx, name)
	env.CreateNodePool(ctx, name, name, tc)
	env.WaitForNodePoolReady(ctx, name)
	return name
}

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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

var _ = DescribeTable("Provisioning",
	func(ctx SpecContext, tc environment.TestCase) {
		runProvisioningTest(ctx, tc)
	},
	// ContainerOptimizedOS
	Entry("COS / amd64 / on-demand", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2"},
		InstanceTypes: []string{"n2-standard-2", "n2-standard-4"},
	}, SpecTimeout(15*time.Minute)),
	Entry("COS / arm64 / on-demand", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureArm64,
		Families:      []string{"t2a"},
		InstanceTypes: []string{"t2a-standard-2", "t2a-standard-4"},
	}, SpecTimeout(15*time.Minute)),
	Entry("COS / amd64 / spot", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeSpot,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2"},
		InstanceTypes: []string{"n2-standard-2", "n2-standard-4"},
	}, SpecTimeout(15*time.Minute)),
	Entry("COS / arm64 / spot", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeSpot,
		Arch:          karpv1.ArchitectureArm64,
		Families:      []string{"t2a"},
		InstanceTypes: []string{"t2a-standard-2", "t2a-standard-4"},
	}, SpecTimeout(15*time.Minute)),
	// Ubuntu
	Entry("Ubuntu / amd64 / on-demand", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2"},
		InstanceTypes: []string{"n2-standard-2", "n2-standard-4"},
		ImageFamily:   gcpv1alpha1.ImageFamilyUbuntu,
	}, SpecTimeout(15*time.Minute)),
	Entry("Ubuntu / arm64 / on-demand", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureArm64,
		Families:      []string{"t2a"},
		InstanceTypes: []string{"t2a-standard-2", "t2a-standard-4"},
		ImageFamily:   gcpv1alpha1.ImageFamilyUbuntu,
	}, SpecTimeout(15*time.Minute)),
	Entry("Ubuntu / amd64 / spot", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeSpot,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2"},
		InstanceTypes: []string{"n2-standard-2", "n2-standard-4"},
		ImageFamily:   gcpv1alpha1.ImageFamilyUbuntu,
	}, SpecTimeout(15*time.Minute)),
	Entry("Ubuntu / arm64 / spot", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeSpot,
		Arch:          karpv1.ArchitectureArm64,
		Families:      []string{"t2a"},
		InstanceTypes: []string{"t2a-standard-2", "t2a-standard-4"},
		ImageFamily:   gcpv1alpha1.ImageFamilyUbuntu,
	}, SpecTimeout(15*time.Minute)),
)

var _ = Describe("Image Pinning", func() {
	It("should provision a node with a pinned ContainerOptimizedOS alias", func(ctx SpecContext) {
		imageURL := env.ResolveCurrentCOSImage(ctx)
		m := cosVersionFromImageRe.FindStringSubmatch(imageURL)
		Expect(m).To(HaveLen(2), "could not extract COS version from %q", imageURL)
		version := strings.ReplaceAll(m[1], "-", ".")
		alias := "ContainerOptimizedOS@" + version
		runPinnedAliasTest(ctx, alias, "cos-"+m[1]+"-c-pre")
	}, SpecTimeout(15*time.Minute))

	It("should provision a node with a pinned Ubuntu alias", func(ctx SpecContext) {
		version := env.ResolveCurrentUbuntuVersion(ctx)
		alias := "Ubuntu@" + version
		runPinnedAliasTest(ctx, alias, version)
	}, SpecTimeout(15*time.Minute))

	It("should provision a node with an exact image id", func(ctx SpecContext) {
		imageURL := env.ResolveCurrentCOSImage(ctx)
		runImageIDTest(ctx, gcpv1alpha1.ImageFamilyContainerOptimizedOS, imageURL)
	}, SpecTimeout(15*time.Minute))
})

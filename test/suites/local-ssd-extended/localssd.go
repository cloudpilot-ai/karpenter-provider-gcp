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

package localssdextended

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	"k8s.io/utils/ptr"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

var (
	env *environment.Environment
	_   = BeforeEach(func() { env = environment.Current() })
)

var _ = DescribeTable("Local SSD extended", Label("suite:local-ssd-extended"),
	func(ctx SpecContext, tc environment.TestCase) {
		env.RunProvisioningTest(ctx, tc)
	},
	Entry("bundled amd64 / RawBlock / 1 SSD", environment.TestCase{
		CapacityType:         karpv1.CapacityTypeOnDemand,
		Arch:                 karpv1.ArchitectureAmd64,
		Families:             []string{"c4", "c4d"},
		InstanceTypes:        []string{"c4-standard-4-lssd", "c4-standard-8-lssd", "c4d-standard-8-lssd"},
		BootDiskCategory:     "hyperdisk-balanced",
		LocalSSDMode:         gcpv1alpha1.LocalSSDModeRawBlock,
		ExpectedScratchDisks: 1,
	}, SpecTimeout(15*time.Minute)),

	Entry("bundled amd64 / Ephemeral / pool count Gt 0", environment.TestCase{
		CapacityType:     karpv1.CapacityTypeOnDemand,
		Arch:             karpv1.ArchitectureAmd64,
		Families:         []string{"c4", "c4d"},
		InstanceTypes:    []string{"c4-standard-4-lssd", "c4-standard-8-lssd", "c4d-standard-8-lssd"},
		BootDiskCategory: "hyperdisk-balanced",
		LocalSSDMode:     gcpv1alpha1.LocalSSDModeEphemeral,
		ExtraRequirements: []map[string]any{{
			"key":      gcpv1alpha1.LabelInstanceLocalSsdCount,
			"operator": "Gt",
			"values":   []any{"0"},
		}},
		ExpectedScratchDisks: 1,
	}, SpecTimeout(15*time.Minute)),

	Entry("family=c4a + pod SSD-count=1 / picks lssd variant", environment.TestCase{
		CapacityType:         karpv1.CapacityTypeOnDemand,
		Arch:                 karpv1.ArchitectureArm64,
		Families:             []string{"c4a"},
		InstanceTypes:        []string{"c4a-standard-2", "c4a-standard-4-lssd", "c4a-highmem-4-lssd"},
		BootDiskCategory:     "hyperdisk-balanced",
		LocalSSDMode:         gcpv1alpha1.LocalSSDModeEphemeral,
		PodLocalSSDCount:     "1",
		ExpectedScratchDisks: 1,
	}, SpecTimeout(15*time.Minute)),

	Entry("NodePool SSD-count=2 / pod has no SSD-count label / n2/n2d gets 2 SSDs", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2", "n2d"},
		InstanceTypes: []string{"n2-standard-2", "n2-standard-4", "n2d-standard-2", "n2d-standard-4"},
		LocalSSDMode:  gcpv1alpha1.LocalSSDModeRawBlock,
		ExtraRequirements: []map[string]any{{
			"key":      gcpv1alpha1.LabelInstanceLocalSsdCount,
			"operator": "In",
			"values":   []any{"2"},
		}},
		ExpectedScratchDisks: 2,
	}, SpecTimeout(15*time.Minute)),

	Entry("static n2/n2d / omitted count / launches count 0", environment.TestCase{
		CapacityType:   karpv1.CapacityTypeOnDemand,
		Arch:           karpv1.ArchitectureAmd64,
		Families:       []string{"n2", "n2d"},
		InstanceTypes:  []string{"n2-standard-2", "n2-standard-4", "n2d-standard-2", "n2d-standard-4"},
		LocalSSDMode:   gcpv1alpha1.LocalSSDModeRawBlock,
		StaticReplicas: ptr.To[int64](1),
	}, SpecTimeout(15*time.Minute)),

	Entry("static n2/n2d / singleton count 2 / launches 2 SSDs", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2", "n2d"},
		InstanceTypes: []string{"n2-standard-2", "n2-standard-4", "n2d-standard-2", "n2d-standard-4"},
		LocalSSDMode:  gcpv1alpha1.LocalSSDModeRawBlock,
		ExtraRequirements: []map[string]any{{
			"key":      gcpv1alpha1.LabelInstanceLocalSsdCount,
			"operator": "In",
			"values":   []any{"2"},
		}},
		ExpectedScratchDisks: 2,
		StaticReplicas:       ptr.To[int64](1),
	}, SpecTimeout(15*time.Minute)),

	Entry("flex n2/n2d / Ubuntu / RawBlock / pool count Gt 0 + pod-set 2 SSDs", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2", "n2d"},
		InstanceTypes: []string{"n2-standard-2", "n2-standard-4", "n2d-standard-2", "n2d-standard-4"},
		ImageFamily:   gcpv1alpha1.ImageFamilyUbuntu,
		LocalSSDMode:  gcpv1alpha1.LocalSSDModeRawBlock,
		ExtraRequirements: []map[string]any{{
			"key":      gcpv1alpha1.LabelInstanceLocalSsdCount,
			"operator": "Gt",
			"values":   []any{"0"},
		}},
		PodLocalSSDCount: "2",
	}, SpecTimeout(15*time.Minute)),

	Entry("flex n2/n2d / Ubuntu / Ephemeral / pool count Gt 0 + pod-set 4 SSDs + 800Gi", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2", "n2d"},
		InstanceTypes: []string{"n2-standard-2", "n2-standard-4", "n2d-standard-2", "n2d-standard-4"},
		ImageFamily:   gcpv1alpha1.ImageFamilyUbuntu,
		LocalSSDMode:  gcpv1alpha1.LocalSSDModeEphemeral,
		ExtraRequirements: []map[string]any{{
			"key":      gcpv1alpha1.LabelInstanceLocalSsdCount,
			"operator": "Gt",
			"values":   []any{"0"},
		}},
		PodLocalSSDCount:     "4",
		PodEphemeralStorage:  "800Gi",
		ExpectedScratchDisks: 4,
	}, SpecTimeout(15*time.Minute)),
)

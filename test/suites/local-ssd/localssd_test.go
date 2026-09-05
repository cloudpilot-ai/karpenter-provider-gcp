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
	"os"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	"k8s.io/utils/ptr"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

var _ = DescribeTable("Local SSD",
	func(ctx SpecContext, tc environment.TestCase) {
		if slices.Contains(tc.Families, "z3") && os.Getenv("E2E_Z3_TESTS") != "true" {
			Skip("set E2E_Z3_TESTS=true to run z3 capacity-constrained tests")
		}
		env.RunProvisioningTest(ctx, tc)
	},
	Entry("bundled c4d-standard-8-lssd / RawBlock", environment.TestCase{
		CapacityType:         karpv1.CapacityTypeOnDemand,
		Arch:                 karpv1.ArchitectureAmd64,
		Families:             []string{"c4d"},
		InstanceTypes:        []string{"c4d-standard-8-lssd"},
		BootDiskCategory:     "hyperdisk-balanced",
		LocalSSDMode:         gcpv1alpha1.LocalSSDModeRawBlock,
		ExpectedScratchDisks: 1,
	}, SpecTimeout(15*time.Minute)),
	Entry("bundled c4d-standard-8-lssd / Ephemeral / pool count Gt 0", environment.TestCase{
		CapacityType:     karpv1.CapacityTypeOnDemand,
		Arch:             karpv1.ArchitectureAmd64,
		Families:         []string{"c4d"},
		InstanceTypes:    []string{"c4d-standard-8-lssd"},
		BootDiskCategory: "hyperdisk-balanced",
		LocalSSDMode:     gcpv1alpha1.LocalSSDModeEphemeral,
		ExtraRequirements: []map[string]any{{
			"key":      gcpv1alpha1.LabelInstanceLocalSsdCount,
			"operator": "Gt",
			"values":   []any{"0"},
		}},
		ExpectedScratchDisks: 1,
	}, SpecTimeout(15*time.Minute)),

	Entry("flex n2d / RawBlock / pool count Gt 0 + pod-set 4 SSDs", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2d"},
		InstanceTypes: []string{"n2d-standard-8"},
		LocalSSDMode:  gcpv1alpha1.LocalSSDModeRawBlock,
		ExtraRequirements: []map[string]any{{
			"key":      gcpv1alpha1.LabelInstanceLocalSsdCount,
			"operator": "Gt",
			"values":   []any{"0"},
		}},
		PodLocalSSDCount: "4",
	}, SpecTimeout(15*time.Minute)),
	Entry("flex n2d / RawBlock / pool count Exists + pod-set 4 SSDs", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2d"},
		InstanceTypes: []string{"n2d-standard-8"},
		LocalSSDMode:  gcpv1alpha1.LocalSSDModeRawBlock,
		ExtraRequirements: []map[string]any{{
			"key":      gcpv1alpha1.LabelInstanceLocalSsdCount,
			"operator": "Exists",
		}},
		PodLocalSSDCount:     "4",
		ExpectedScratchDisks: 4,
	}, SpecTimeout(15*time.Minute)),

	Entry("flex n2d / no SSD-count label / zero SSDs", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2d"},
		InstanceTypes: []string{"n2d-standard-2"},
		LocalSSDMode:  gcpv1alpha1.LocalSSDModeRawBlock,
	}, SpecTimeout(15*time.Minute)),

	Entry("family=c4 + no SSD-count label / picks no-SSD variant", environment.TestCase{
		CapacityType:     karpv1.CapacityTypeOnDemand,
		Arch:             karpv1.ArchitectureAmd64,
		Families:         []string{"c4"},
		InstanceTypes:    []string{"c4-standard-2", "c4-standard-8-lssd"},
		BootDiskCategory: "hyperdisk-balanced",
		LocalSSDMode:     gcpv1alpha1.LocalSSDModeEphemeral,
	}, SpecTimeout(15*time.Minute)),
	Entry("family=c4a + pod SSD-count=1 / picks lssd variant", environment.TestCase{
		CapacityType:         karpv1.CapacityTypeOnDemand,
		Arch:                 karpv1.ArchitectureArm64,
		Families:             []string{"c4a"},
		InstanceTypes:        []string{"c4a-standard-2", "c4a-standard-4-lssd"},
		BootDiskCategory:     "hyperdisk-balanced",
		LocalSSDMode:         gcpv1alpha1.LocalSSDModeEphemeral,
		PodLocalSSDCount:     "1",
		ExpectedScratchDisks: 1,
	}, SpecTimeout(15*time.Minute)),

	Entry("NodePool SSD-count=4 / pod has no SSD-count label / n2d gets 4 SSDs", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2d"},
		InstanceTypes: []string{"n2d-standard-8"},
		LocalSSDMode:  gcpv1alpha1.LocalSSDModeRawBlock,
		ExtraRequirements: []map[string]any{{
			"key":      gcpv1alpha1.LabelInstanceLocalSsdCount,
			"operator": "In",
			"values":   []any{"4"},
		}},
		ExpectedScratchDisks: 4,
	}, SpecTimeout(15*time.Minute)),
	Entry("static n2d / omitted count / launches count 0", environment.TestCase{
		CapacityType:   karpv1.CapacityTypeOnDemand,
		Arch:           karpv1.ArchitectureAmd64,
		Families:       []string{"n2d"},
		InstanceTypes:  []string{"n2d-standard-2"},
		LocalSSDMode:   gcpv1alpha1.LocalSSDModeRawBlock,
		StaticReplicas: ptr.To[int64](1),
	}, SpecTimeout(15*time.Minute)),
	Entry("static n2d / singleton count 4 / launches 4 SSDs", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2d"},
		InstanceTypes: []string{"n2d-standard-8"},
		LocalSSDMode:  gcpv1alpha1.LocalSSDModeRawBlock,
		ExtraRequirements: []map[string]any{{
			"key":      gcpv1alpha1.LabelInstanceLocalSsdCount,
			"operator": "In",
			"values":   []any{"4"},
		}},
		ExpectedScratchDisks: 4,
		StaticReplicas:       ptr.To[int64](1),
	}, SpecTimeout(15*time.Minute)),

	Entry("flex n2d / Ephemeral / pool count Gt 0 + pod-set 4 SSDs + 800Gi", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2d"},
		InstanceTypes: []string{"n2d-standard-8"},
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

	Entry("flex n2d / RawBlock / NodePool count In:[2,4] + pod-set 4 SSDs", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2d"},
		InstanceTypes: []string{"n2d-standard-8"},
		LocalSSDMode:  gcpv1alpha1.LocalSSDModeRawBlock,
		ExtraRequirements: []map[string]any{{
			"key":      gcpv1alpha1.LabelInstanceLocalSsdCount,
			"operator": "In",
			"values":   []any{"2", "4"},
		}},
		PodLocalSSDCount:     "4",
		ExpectedScratchDisks: 4,
	}, SpecTimeout(15*time.Minute)),

	Entry("family=c4 / NodePool count In:[0] excludes bundled lssd SKU", environment.TestCase{
		CapacityType:     karpv1.CapacityTypeOnDemand,
		Arch:             karpv1.ArchitectureAmd64,
		Families:         []string{"c4"},
		InstanceTypes:    []string{"c4-standard-2", "c4-standard-8-lssd"},
		BootDiskCategory: "hyperdisk-balanced",
		LocalSSDMode:     gcpv1alpha1.LocalSSDModeEphemeral,
		ExtraRequirements: []map[string]any{{
			"key":      gcpv1alpha1.LabelInstanceLocalSsdCount,
			"operator": "In",
			"values":   []any{"0"},
		}},
	}, SpecTimeout(15*time.Minute)),

	Entry("flex n2d / Ubuntu / RawBlock / pool count Gt 0 + pod-set 4 SSDs", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2d"},
		InstanceTypes: []string{"n2d-standard-8"},
		ImageFamily:   gcpv1alpha1.ImageFamilyUbuntu,
		LocalSSDMode:  gcpv1alpha1.LocalSSDModeRawBlock,
		ExtraRequirements: []map[string]any{{
			"key":      gcpv1alpha1.LabelInstanceLocalSsdCount,
			"operator": "Gt",
			"values":   []any{"0"},
		}},
		PodLocalSSDCount: "4",
	}, SpecTimeout(15*time.Minute)),
	Entry("flex n2d / Ubuntu / Ephemeral / pool count Gt 0 + pod-set 4 SSDs + 800Gi", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2d"},
		InstanceTypes: []string{"n2d-standard-8"},
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

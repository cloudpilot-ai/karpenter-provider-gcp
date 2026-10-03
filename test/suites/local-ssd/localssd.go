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

package localssd

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

var (
	env *environment.Environment
	_   = BeforeEach(func() { env = environment.Current() })
)

var _ = DescribeTable("Local SSD", Label("suite:local-ssd"),
	func(ctx SpecContext, tc environment.TestCase) {
		env.RunProvisioningTest(ctx, tc)
	},
	Entry("flex n2/n2d / COS / RawBlock / pool count Gt 0 + pod-set 2 SSDs", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2", "n2d"},
		InstanceTypes: []string{"n2-standard-2", "n2-standard-4", "n2d-standard-2", "n2d-standard-4"},
		LocalSSDMode:  gcpv1alpha1.LocalSSDModeRawBlock,
		ExtraRequirements: []map[string]any{{
			"key":      gcpv1alpha1.LabelInstanceLocalSsdCount,
			"operator": "Gt",
			"values":   []any{"0"},
		}},
		PodLocalSSDCount: "2",
	}, SpecTimeout(15*time.Minute)),

	Entry("flex n2/n2d / Ephemeral / no SSD-count label / zero SSDs", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2", "n2d"},
		InstanceTypes: []string{"n2-standard-2", "n2-standard-4", "n2d-standard-2", "n2d-standard-4"},
		LocalSSDMode:  gcpv1alpha1.LocalSSDModeEphemeral,
	}, SpecTimeout(15*time.Minute)),

	Entry("flex n2/n2d / COS / Ephemeral / pool count Gt 0 + pod-set 4 SSDs + 800Gi", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2", "n2d"},
		InstanceTypes: []string{"n2-standard-2", "n2-standard-4", "n2d-standard-2", "n2d-standard-4"},
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

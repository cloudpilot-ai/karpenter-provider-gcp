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
	"time"

	. "github.com/onsi/ginkgo/v2"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

var _ = Describe("On-Demand Provisioning", func() {
	It("should provision an amd64 on-demand node", func(ctx SpecContext) {
		runProvisioningTest(ctx, environment.TestCase{
			CapacityType: karpv1.CapacityTypeOnDemand,
			Arch:         karpv1.ArchitectureAmd64,
			// e2-medium is the smallest cost-effective AMD64 instance; n2/e2-standard-2
			// are listed as fallbacks for regions where e2-medium capacity is tight.
			Families:      []string{"e2", "n2"},
			InstanceTypes: []string{"e2-medium", "e2-standard-2", "n2-standard-2"},
		})
	}, SpecTimeout(15*time.Minute))

	XIt("should provision an arm64 on-demand node", func(ctx SpecContext) {
		runProvisioningTest(ctx, environment.TestCase{
			CapacityType: karpv1.CapacityTypeOnDemand,
			Arch:         karpv1.ArchitectureArm64,
			// c4a (Google Axion) is available in more regions than t2a; both are listed
			// so the test succeeds in zones that only have one of the two families.
			Families:      []string{"c4a", "t2a"},
			InstanceTypes: []string{"c4a-standard-2", "c4a-standard-4", "t2a-standard-2"},
		})
	}, SpecTimeout(15*time.Minute))
})

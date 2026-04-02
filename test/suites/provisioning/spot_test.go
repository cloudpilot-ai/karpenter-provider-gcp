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
)

var _ = Describe("Spot Provisioning", func() {
	It("should provision an amd64 spot node", func(ctx SpecContext) {
		runProvisioningTest(ctx, provisioningCase{
			capacityType:  karpv1.CapacityTypeSpot,
			arch:          karpv1.ArchitectureAmd64,
			families:      []string{"e2", "n2"},
			instanceTypes: []string{"e2-standard-2", "e2-standard-4", "n2-standard-2", "n2-standard-4"},
		})
	}, SpecTimeout(25*time.Minute))

	It("should provision an arm64 spot node", func(ctx SpecContext) {
		runProvisioningTest(ctx, provisioningCase{
			capacityType:  karpv1.CapacityTypeSpot,
			arch:          karpv1.ArchitectureArm64,
			families:      []string{"t2a"},
			instanceTypes: []string{"t2a-standard-2"},
		})
	}, SpecTimeout(25*time.Minute))
})

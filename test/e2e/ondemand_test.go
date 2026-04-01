package e2e

import (
	"testing"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

func TestOnDemandAMD64(t *testing.T) {
	runProvisioningTest(t, provisioningCase{
		name:          "ondemand-amd64",
		capacityType:  karpv1.CapacityTypeOnDemand,
		arch:          karpv1.ArchitectureAmd64,
		families:      []string{"e2"},
		instanceTypes: []string{"e2-small"},
	})
}

func TestOnDemandARM64(t *testing.T) {
	runProvisioningTest(t, provisioningCase{
		name:          "ondemand-arm64",
		capacityType:  karpv1.CapacityTypeOnDemand,
		arch:          karpv1.ArchitectureArm64,
		families:      []string{"t2a"},
		instanceTypes: []string{"t2a-standard-1"},
	})
}

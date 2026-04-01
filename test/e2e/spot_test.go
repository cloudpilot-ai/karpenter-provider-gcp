package e2e

import (
	"testing"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

func TestSpotAMD64(t *testing.T) {
	runProvisioningTest(t, provisioningCase{
		name:          "spot-amd64",
		capacityType:  karpv1.CapacityTypeSpot,
		arch:          karpv1.ArchitectureAmd64,
		families:      []string{"e2"},
		instanceTypes: []string{"e2-small"},
	})
}

func TestSpotARM64(t *testing.T) {
	runProvisioningTest(t, provisioningCase{
		name:          "spot-arm64",
		capacityType:  karpv1.CapacityTypeSpot,
		arch:          karpv1.ArchitectureArm64,
		families:      []string{"t2a"},
		instanceTypes: []string{"t2a-standard-1"},
	})
}

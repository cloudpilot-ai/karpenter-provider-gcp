package e2e

import karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

func spotCases() []provisioningCase {
	return []provisioningCase{
		{
			name:         "TestSpotAMD64",
			capacityType: karpv1.CapacityTypeSpot,
			arch:         karpv1.ArchitectureAmd64,
			families:     []string{"n2", "e2", "n4"},
		},
		{
			name:         "TestSpotARM64",
			capacityType: karpv1.CapacityTypeSpot,
			arch:         karpv1.ArchitectureArm64,
			families:     []string{"t2a"},
		},
	}
}

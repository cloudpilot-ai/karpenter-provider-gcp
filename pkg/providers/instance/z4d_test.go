/*
Copyright 2026 The CloudPilot AI Authors.

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

package instance

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

func TestBuildInstanceZ4DMaintenance(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		shape      string
		partitions int32
		policy     string
	}{
		{"smallest shape", "z4d-highmem-8-highlssd", 1, "MIGRATE"},
		{"42000 GiB boundary", "z4d-highmem-192-standardlssd", 12, "MIGRATE"},
		{"84000 GiB highlssd", "z4d-highmem-192-highlssd", 24, "TERMINATE"},
		{"84000 GiB standardlssd", "z4d-highmem-384-standardlssd", 24, "TERMINATE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			instanceType := &cloudprovider.InstanceType{
				Name: tc.shape,
				Requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"),
					scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpDoesNotExist),
				),
				Overhead: &cloudprovider.InstanceTypeOverhead{KubeReserved: corev1.ResourceList{}},
			}
			nodeClass := bootDiskNodeClass("hyperdisk-balanced", nil, nil)
			instance, err := makeProvider().buildInstance(
				context.Background(), spotOrOnDemandNodeClaim(), nodeClass, instanceType,
				makeSourceMetadata("max-pods-per-node=110,max-pods=110,disk-type.gke.io/pd-balanced=true"),
				makeCluster("projects/p/global/networks/my-vpc", "regions/us-central1/subnetworks/my-subnet", "pods", false),
				"us-central1-a", "z4d-test", karpv1.CapacityTypeOnDemand,
				machineTypeWithBundledSSDs(tc.partitions), int(tc.partitions),
			)
			require.NoError(t, err)
			require.Equal(t, tc.policy, instance.Scheduling.OnHostMaintenance)
			require.Len(t, instance.Disks, 1, "bundled SSDs must not be attached explicitly")
			require.Contains(t, instance.Disks[0].InitializeParams.DiskType, "/diskTypes/hyperdisk-balanced")
			for _, value := range []string{kubeLabelsFrom(t, instance), kubeEnvFrom(t, instance)} {
				require.Contains(t, value, "disk-type.gke.io/hyperdisk-balanced=true")
				require.Contains(t, value, "disk-type.gke.io/hyperdisk-throughput=true")
				require.NotContains(t, value, "disk-type.gke.io/pd-balanced=true")
			}
		})
	}
}

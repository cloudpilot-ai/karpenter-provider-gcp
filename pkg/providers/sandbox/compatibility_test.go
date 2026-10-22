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

package sandbox

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHardwarePolicyCoversCPUCustomAndGPUFamilies(t *testing.T) {
	for _, name := range []string{"n1-standard-2", "custom-4-16384", "e2-custom-4-16384", "n2-custom-4-16384", "c2-standard-4", "c3-highcpu-4", "c4-standard-4", "n4-standard-4", "h3-standard-88", "z3-standard-8", "m1-ultramem-40", "m2-ultramem-208", "m3-ultramem-32", "m4-megamem-28", "x4-megamem-960", "a2-highgpu-1g", "a3-highgpu-8g", "a4-highgpu-8g", "g2-standard-4"} {
		policy, err := ResolveHardwarePolicy(name, "amd64")
		require.NoError(t, err, name)
		require.True(t, policy.CoreTags, name)
		require.Equal(t, "amd64-core-tags", policy.Variant)
	}
	for _, name := range []string{"n2d-custom-4-16384", "n4d-custom-4-16384", "c2d-standard-4", "c3d-standard-4", "c4d-standard-4", "t2d-standard-4", "h4d-standard-192", "z4d-standard-8", "g4-standard-48"} {
		policy, err := ResolveHardwarePolicy(name, "amd64")
		require.NoError(t, err, name)
		require.False(t, policy.CoreTags, name)
		require.Equal(t, "amd64-amd", policy.Variant)
	}
	for _, name := range []string{"t2a-standard-4", "c4a-standard-4", "n4a-standard-4", "a4x-highgpu-4g"} {
		policy, err := ResolveHardwarePolicy(name, "arm64")
		require.NoError(t, err)
		require.False(t, policy.CoreTags)
		require.Equal(t, "arm64", policy.Variant)
	}
	_, err := ResolveHardwarePolicy("n2-standard-2", "unknown")
	require.Error(t, err)
}

func TestGPUCompatibilityPatchFloorsAndBothEffectiveReleases(t *testing.T) {
	for _, pair := range []struct{ model, below, floor string }{
		{"nvidia-tesla-t4", "1.29.15-gke.1133999", "1.29.15-gke.1134000"},
		{"nvidia-l4", "1.30.11-gke.1092999", "1.30.11-gke.1093000"},
		{"nvidia-tesla-a100", "1.31.7-gke.1148999", "1.31.7-gke.1149000"},
		{"nvidia-a100-80gb", "1.32.2-gke.1182002", "1.32.2-gke.1182003"},
		{"nvidia-h100-80gb", "1.29.14-gke.9999999", "1.29.15-gke.1134000"},
		{"nvidia-h200-141gb", "1.34.0-gke.1712999", "1.34.0-gke.1713000"},
		{"nvidia-b200", "1.34.0-gke.1712999", "1.34.0-gke.1713000"},
		{"nvidia-gb200", "1.33.9-gke.9999999", "1.34.0-gke.1713000"},
		{"nvidia-rtx-pro-6000", "1.34.1-gke.2037000", "1.34.1-gke.2037001"},
	} {
		t.Run(pair.model, func(t *testing.T) {
			require.NoError(t, GPUCompatible(pair.model, "default", 1, false, pair.floor, "v"+pair.floor))
			require.Error(t, GPUCompatible(pair.model, "latest", 1, false, pair.below, pair.floor))
			require.Error(t, GPUCompatible(pair.model, "latest", 1, false, pair.floor, pair.below))
			require.NoError(t, GPUCompatible(pair.model, "latest", 8, false, "1.35.6-gke.1250001", "v1.35.6-gke.1250001"))
		})
	}
	for _, model := range []string{"nvidia-tesla-v100", "nvidia-tesla-p100", "nvidia-unknown", "nvidia-l4-vws"} {
		require.Error(t, GPUCompatible(model, "default", 1, false, "1.35.6-gke.1250001", "1.35.6-gke.1250001"))
	}
	require.Error(t, GPUCompatible("nvidia-l4", "disabled", 1, false, "1.35.6-gke.1250001", "1.35.6-gke.1250001"))
	require.Error(t, GPUCompatible("nvidia-l4", "default", 0, false, "1.35.6-gke.1250001", "1.35.6-gke.1250001"))
	require.Error(t, GPUCompatible("nvidia-l4", "default", 1, true, "1.35.6-gke.1250001", "1.35.6-gke.1250001"))
	require.Error(t, GPUCompatible("nvidia-l4", "default", 1, false, "latest", "1.35.6-gke.1250001"))
}

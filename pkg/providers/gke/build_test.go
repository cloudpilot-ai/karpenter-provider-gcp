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

package gke

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCompareGKEReleasesComparesBuildNumbersNumerically(t *testing.T) {
	for _, tc := range []struct {
		name       string
		a, b       string
		wantBefore bool
	}{
		{name: "lower build is older", a: "v1.34.1-gke.999", b: "v1.34.1-gke.1341000", wantBefore: true},
		{name: "higher build is newer", a: "v1.35.8-gke.1796000", b: "v1.34.1-gke.1341000"},
		{name: "patch version dominates build", a: "v1.34.2-gke.1", b: "v1.34.1-gke.1341000"},
		{name: "minor version dominates patch", a: "v1.35.0-gke.1", b: "v1.34.9-gke.1341000"},
		{name: "equal releases", a: "v1.34.1-gke.1341000", b: "1.34.1-gke.1341000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			order, err := CompareGKEReleases(tc.a, tc.b)
			require.NoError(t, err)
			require.Equal(t, tc.wantBefore, order < 0, "CompareGKEReleases(%q, %q) = %d", tc.a, tc.b, order)
		})
	}
}

func TestGKEReleaseAtLeast(t *testing.T) {
	supported, err := GKEReleaseAtLeast("v1.34.1-gke.1341000", "v1.34.1-gke.1341000")
	require.NoError(t, err)
	require.True(t, supported)

	supported, err = GKEReleaseAtLeast("v1.35.8-gke.1796000", "v1.34.1-gke.1341000")
	require.NoError(t, err)
	require.True(t, supported)

	supported, err = GKEReleaseAtLeast("v1.34.1-gke.999", "v1.34.1-gke.1341000")
	require.NoError(t, err)
	require.False(t, supported, "a smaller GKE build number is an older release")

	supported, err = GKEReleaseAtLeast("v1.30.1-gke.123", "v1.34.1-gke.1341000")
	require.NoError(t, err)
	require.False(t, supported)

	for _, unknown := range []string{"", "latest", "1.34", "v1.34.1-gke", "v1.34.1-gke.abc"} {
		_, err := GKEReleaseAtLeast(unknown, "v1.34.1-gke.1341000")
		require.ErrorContains(t, err, "unknown GKE release version", "version %q must not be assumed supported", unknown)
	}
}

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

package environment

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewerCOSImagePrefixesRequireSkewFromEverySourceWithinMasterMinor(t *testing.T) {
	prefixes, err := newerCOSImagePrefixes("v1.36.5-gke.1", map[string]string{
		"older": "1.36.4-gke.8", "newest": "1.36.4-gke.9",
	}, []string{
		"1.35.13-gke.100", "1.36.4-gke.8", "1.36.4-gke.9",
		"v1.36.4-gke.10", "1.36.5-gke.1", "1.36.5-gke.2", "1.36.6-gke.1", "1.37.0-gke.100",
	})
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"gke-1364-gke10-cos-": "1.36.4-gke.10",
		"gke-1365-gke1-cos-":  "1.36.5-gke.1",
	}, prefixes, "equal releases, a different minor and lexicographic build ordering must not create false skew")
}

func TestNewerCOSImagePrefixesCannotAdvanceBeyondMaster(t *testing.T) {
	prefixes, err := newerCOSImagePrefixes("1.36.4-gke.9", map[string]string{"source": "1.36.4-gke.9"}, []string{"1.36.4-gke.10"})
	require.NoError(t, err)
	require.Empty(t, prefixes, "a source already at the master release leaves no supported skew fixture")
}

func TestNewerCOSImagePrefixesRejectMissingOrMalformedSourceEvidence(t *testing.T) {
	for _, sources := range []map[string]string{nil, {"source": ""}, {"source": "unknown"}} {
		_, err := newerCOSImagePrefixes("1.36.4-gke.9", sources, []string{"1.36.5-gke.1"})
		require.Error(t, err)
	}
}

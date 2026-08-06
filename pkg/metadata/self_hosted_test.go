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

package metadata

import (
	"strings"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
)

func TestSelfHostedComputeMetadata_RendersOnlyOwnedAndCustomKeys(t *testing.T) {
	t.Parallel()

	// Multi-line script with trailing newline; must be written byte-for-byte.
	startupScript := "#!/bin/bash\nset -euo pipefail\necho 'nodeup: bootstrap'\n"
	custom := CustomMetadata{
		"kops-k8s-io-instance-group-name": "nodes-us-central1-a",
		"":                                "dropped-empty-key",
	}

	result := SelfHostedComputeMetadata("poc0006.k8s.local", startupScript, custom)

	values := map[string]string{}
	for _, item := range result.Items {
		values[item.Key] = lo.FromPtr(item.Value)
	}
	require.Equal(t, map[string]string{
		ClusterNameKey:                    "poc0006.k8s.local",
		StartupScriptKey:                  startupScript,
		"kops-k8s-io-instance-group-name": "nodes-us-central1-a",
	}, values)
	require.NotContains(t, values, KubeEnvKey)
	require.NotContains(t, values, KubeLabelsKey)
	require.NotContains(t, values, KubeletConfigKey)
}

func TestSelfHostedComputeMetadata_OmitsStartupScriptWhenEmpty(t *testing.T) {
	t.Parallel()

	// Images with baked-in bootstrap need no startup script; the key must be absent
	// entirely, not rendered with an empty value.
	result := SelfHostedComputeMetadata("poc0006.k8s.local", "", CustomMetadata{
		"kops-k8s-io-instance-group-name": "nodes-us-central1-a",
	})

	values := map[string]string{}
	for _, item := range result.Items {
		values[item.Key] = lo.FromPtr(item.Value)
	}
	require.NotContains(t, values, StartupScriptKey)
	require.Equal(t, "poc0006.k8s.local", values[ClusterNameKey])
	require.Equal(t, "nodes-us-central1-a", values["kops-k8s-io-instance-group-name"])
}

func TestSelfHostedComputeMetadata_ProviderOwnedKeysWinOverCustom(t *testing.T) {
	t.Parallel()

	// spec.metadata cannot contain reserved keys (CRD CEL), but the builder must still
	// be safe if that invariant is ever bypassed.
	result := SelfHostedComputeMetadata("exact-name", "#!/bin/bash\n", CustomMetadata{
		ClusterNameKey:   "spoofed",
		StartupScriptKey: "spoofed",
	})

	values := map[string]string{}
	for _, item := range result.Items {
		values[item.Key] = lo.FromPtr(item.Value)
	}
	require.Equal(t, "exact-name", values[ClusterNameKey])
	require.Equal(t, "#!/bin/bash\n", values[StartupScriptKey])
}

func TestValidateComputeMetadataSize(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name            string
		startupScript   string
		custom          CustomMetadata
		wantErrContains []string
	}{
		{
			name:          "under limits",
			startupScript: strings.Repeat("x", MaxMetadataValueBytes),
		},
		{
			name:          "single value over limit",
			startupScript: strings.Repeat("x", MaxMetadataValueBytes+1),
			wantErrContains: []string{
				`metadata value for key "startup-script"`,
				"per-value limit of 262144 bytes",
			},
		},
		{
			// Each value is at the per-value limit, so only the combined total trips.
			name:          "total over limit",
			startupScript: strings.Repeat("x", MaxMetadataValueBytes),
			custom: CustomMetadata{
				"user-payload": strings.Repeat("y", MaxMetadataValueBytes),
			},
			wantErrContains: []string{"per-instance limit of 524288 bytes"},
		},
		{
			// CRD maxLength counts characters: 70000 four-byte runes pass the
			// 131072-char CEL limit on startupScript but total 280000 bytes, over
			// GCE's 262144-byte limit.
			name:            "multi-byte script under char limit over byte limit",
			startupScript:   strings.Repeat("\U0001F680", 70000),
			wantErrContains: []string{`metadata value for key "startup-script" is 280000 bytes`},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			md := SelfHostedComputeMetadata("cluster", tc.startupScript, tc.custom)

			err := ValidateComputeMetadataSize(md)
			if len(tc.wantErrContains) == 0 {
				require.NoError(t, err)
				return
			}
			for _, want := range tc.wantErrContains {
				require.ErrorContains(t, err, want)
			}
		})
	}
}

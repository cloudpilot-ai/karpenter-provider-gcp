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

package options

import (
	"flag"
	"testing"

	"github.com/stretchr/testify/require"
	coreoptions "sigs.k8s.io/karpenter/pkg/operator/options"
)

func parseOptions(t *testing.T, args ...string) (*Options, error) {
	t.Helper()
	fs := &coreoptions.FlagSet{FlagSet: flag.NewFlagSet("test", flag.ContinueOnError)}
	opts := &Options{}
	opts.AddFlags(fs)
	err := opts.Parse(fs, args...)
	return opts, err
}

func requiredArgs() []string {
	return []string{"--project-id=my-project", "--cluster-name=my-cluster", "--cluster-location=us-central1"}
}

func TestParse_ProvisionModeSelection(t *testing.T) {
	t.Parallel()

	opts, err := parseOptions(t, requiredArgs()...)
	require.NoError(t, err)
	require.Equal(t, ProvisionModeGKE, opts.ProvisionMode)
	require.False(t, opts.IsSelfHosted())

	opts, err = parseOptions(t, append(requiredArgs(), "--provision-mode=self-hosted")...)
	require.NoError(t, err)
	require.True(t, opts.IsSelfHosted())
}

func TestParse_Validation(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name            string
		args            []string
		wantErrContains string
	}{
		{
			name:            "unknown provision mode rejected",
			args:            append(requiredArgs(), "--provision-mode=aks"),
			wantErrContains: `invalid value "aks"`,
		},
		{
			name: "self-hosted rejects default-nodepool-template-name",
			args: append(requiredArgs(),
				"--provision-mode=self-hosted", "--default-nodepool-template-name=default-pool"),
			wantErrContains: "default-nodepool-template-name",
		},
		{
			name: "gke mode allows default-nodepool-template-name",
			args: append(requiredArgs(), "--default-nodepool-template-name=default-pool"),
		},
		{
			name: "self-hosted accepts FQDN cluster name",
			args: []string{"--project-id=my-project", "--cluster-name=poc0006.k8s.local",
				"--cluster-location=us-central1", "--provision-mode=self-hosted"},
		},
		{
			name: "self-hosted rejects cluster name sanitizing to empty",
			args: []string{"--project-id=my-project", "--cluster-name=---",
				"--cluster-location=us-central1", "--provision-mode=self-hosted"},
			wantErrContains: "sanitizes to an empty GCE label value",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := parseOptions(t, tc.args...)
			if tc.wantErrContains == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErrContains)
		})
	}
}

func TestParse_ClusterLocationNormalization(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name             string
		provisionMode    string
		clusterLocation  string
		expectedLocation string
	}{
		{
			name:             "self-hosted zonal location becomes region",
			provisionMode:    ProvisionModeSelfHosted,
			clusterLocation:  "us-central1-a",
			expectedLocation: "us-central1",
		},
		{
			name:             "self-hosted two-digit region zone",
			provisionMode:    ProvisionModeSelfHosted,
			clusterLocation:  "europe-west10-a",
			expectedLocation: "europe-west10",
		},
		{
			name:             "self-hosted regional location unchanged",
			provisionMode:    ProvisionModeSelfHosted,
			clusterLocation:  "us-central1",
			expectedLocation: "us-central1",
		},
		{
			name:             "gke mode keeps zonal location",
			provisionMode:    ProvisionModeGKE,
			clusterLocation:  "us-central1-a",
			expectedLocation: "us-central1-a",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			opts, err := parseOptions(t,
				"--project-id=my-project", "--cluster-name=my-cluster",
				"--cluster-location="+tc.clusterLocation, "--provision-mode="+tc.provisionMode)
			require.NoError(t, err)
			require.Equal(t, tc.expectedLocation, opts.ClusterLocation)
			require.Equal(t, tc.expectedLocation, opts.NodeLocation)
		})
	}
}

func TestIsSelfHosted_NilReceiverDefaultsToGKE(t *testing.T) {
	t.Parallel()

	var opts *Options

	require.False(t, opts.IsSelfHosted())
}

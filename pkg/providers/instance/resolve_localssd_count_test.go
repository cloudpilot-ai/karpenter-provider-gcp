/*
Copyright 2025 The CloudPilot AI Authors.

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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

func TestResolveCreateSSDCount(t *testing.T) {
	t.Parallel()

	variant := func(reqs ...*scheduling.Requirement) *cloudprovider.InstanceType {
		r := scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, "n2d-standard-8"),
		)
		r.Add(reqs...)
		return &cloudprovider.InstanceType{Name: "n2d-standard-8", Requirements: r}
	}
	countReq := func(values ...string) *scheduling.Requirement {
		return scheduling.NewRequirement(v1alpha1.LabelInstanceLocalSsdCount, corev1.NodeSelectorOpIn, values...)
	}

	cases := []struct {
		name    string
		it      *cloudprovider.InstanceType
		want    int
		wantErr bool
	}{
		{
			name: "configurable count 0 variant",
			it:   variant(countReq("0")),
			want: 0,
		},
		{
			name: "configurable count 4 variant",
			it:   variant(countReq("4")),
			want: 4,
		},
		{
			name: "bundled fixed count variant",
			it:   variant(countReq("2")),
			want: 2,
		},
		{
			name:    "missing count requirement is rejected",
			it:      variant(),
			wantErr: true,
		},
		{
			name:    "multi-valued variant requirement is rejected",
			it:      variant(countReq("2", "4")),
			wantErr: true,
		},
		{
			name:    "non-integer variant value is rejected",
			it:      variant(countReq("bogus")),
			wantErr: true,
		},
		{
			name:    "negative variant value is rejected",
			it:      variant(countReq("-1")),
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveCreateSSDCount(tc.it)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

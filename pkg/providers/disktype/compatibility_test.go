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

package disktype

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFamily(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name         string
		instanceType string
		want         string
	}{
		{name: "standard", instanceType: "n2-standard-4", want: "n2"},
		{name: "highcpu", instanceType: "c3-highcpu-22", want: "c3"},
		{name: "custom", instanceType: "n2-custom-4-8192", want: "n2"},
		{name: "single token", instanceType: "n2", want: "n2"},
		{name: "empty", instanceType: "", want: ""},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, Family(tc.instanceType))
		})
	}
}

func TestLabelsForFamily(t *testing.T) {
	t.Parallel()

	labels, ok := LabelsForFamily("n2")
	require.True(t, ok)
	require.Equal(t, "true", labels["disk-type.gke.io/pd-balanced"])
	require.Equal(t, "true", labels["disk-type.gke.io/pd-ssd"])
	require.Equal(t, "true", labels["disk-type.gke.io/hyperdisk-throughput"])
}

func TestLabelsForInstanceType(t *testing.T) {
	t.Parallel()

	labels, ok := LabelsForInstanceType("e2-standard-4")
	require.True(t, ok)
	require.Equal(t, map[string]string{
		"disk-type.gke.io/pd-balanced": "true",
		"disk-type.gke.io/pd-extreme":  "true",
		"disk-type.gke.io/pd-ssd":      "true",
		"disk-type.gke.io/pd-standard": "true",
	}, labels)
}

func TestLabelsForUnknownFamily(t *testing.T) {
	t.Parallel()

	labels, ok := LabelsForInstanceType("unknown-standard-4")
	require.False(t, ok)
	require.Nil(t, labels)
}

func TestLabelsForFamilyReturnsCopy(t *testing.T) {
	t.Parallel()

	labels, ok := LabelsForFamily("n2")
	require.True(t, ok)
	delete(labels, "disk-type.gke.io/pd-balanced")

	labelsAgain, ok := LabelsForFamily("n2")
	require.True(t, ok)
	require.Equal(t, "true", labelsAgain["disk-type.gke.io/pd-balanced"])
}

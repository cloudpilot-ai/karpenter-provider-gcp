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

package kubelet_config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCountCPUs(t *testing.T) {
	for list, want := range map[string]int{"2": 1, "0-3": 4, "0-1,6": 3, "0-3,6-7": 6} {
		got, err := countCPUs(list)
		require.NoError(t, err, list)
		require.Equal(t, want, got, list)
	}
	for _, list := range []string{"", "a", "3-1", "1-", "1,,2"} {
		_, err := countCPUs(list)
		require.Error(t, err, list)
	}
}

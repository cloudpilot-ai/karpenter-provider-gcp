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
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseLabelsCanonicalizes(t *testing.T) {
	labels := ParseLabels("b=2,,a=1,broken,c=3,")

	require.Equal(t, "a=1,b=2,c=3", labels.String())
}

func TestParseLabelsDuplicateLastWins(t *testing.T) {
	labels := ParseLabels("a=old,a=new")

	require.Equal(t, "new", labels["a"])
	require.Equal(t, "a=new", labels.String())
}

func TestParseLabelsTrimsAndSkipsMalformedEntries(t *testing.T) {
	labels := ParseLabels(" a = 1 , =missing-key , broken , b=2 , c= ")

	require.Equal(t, Labels{"a": "1", "b": "2", "c": ""}, labels)
}

func TestLabelsStringCanonicalizes(t *testing.T) {
	labels := Labels{"b": "2", "a": "1", "c": "3"}

	require.Equal(t, "a=1,b=2,c=3", labels.String())
}

func TestMergeLabels(t *testing.T) {
	labels := Labels{"a": "old", "b": "2"}

	mergeLabels(labels, Labels{"a": "new", "c": "", "": "ignored"})
	delete(labels, "b")

	require.Equal(t, "new", labels["a"])
	require.Equal(t, "", labels["b"])
	require.Equal(t, "", labels["c"])
	require.Equal(t, "a=new,c=", labels.String())
}

func TestMergeLabelsNilDestinationNoPanic(t *testing.T) {
	var labels Labels

	require.NotPanics(t, func() {
		mergeLabels(labels, Labels{"a": "1"})
	})
	require.Empty(t, labels.String())
}

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

package repair

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/watch"
)

func TestReplacementOrdering(t *testing.T) {
	originalDeleting := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"uid": "original", "deletionTimestamp": "2026-01-01T00:05:00Z"},
	}}
	replacementPending := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"uid": "replacement"},
	}}
	replacementInitialized := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"uid": "replacement"},
		"status": map[string]any{"nodeName": "replacement-node", "conditions": []any{
			map[string]any{"type": "Initialized", "status": "True"},
		}},
	}}
	for _, tc := range []struct {
		name      string
		events    []watch.Event
		wantError string
	}{
		{
			name: "original deletion before replacement initialization fails",
			events: []watch.Event{
				{Type: watch.Added, Object: replacementPending},
				{Type: watch.Modified, Object: originalDeleting},
			},
			wantError: "before replacement initialization",
		},
		{
			name: "replacement initializes before original deletion",
			events: []watch.Event{
				{Type: watch.Added, Object: replacementPending},
				{Type: watch.Modified, Object: replacementInitialized},
				{Type: watch.Modified, Object: originalDeleting},
			},
		},
		{
			name: "deleted replacement does not count as initialized capacity",
			events: []watch.Event{
				{Type: watch.Added, Object: replacementPending},
				{Type: watch.Modified, Object: replacementInitialized},
				{Type: watch.Deleted, Object: replacementInitialized},
				{Type: watch.Deleted, Object: originalDeleting},
			},
			wantError: "before replacement initialization",
		},
		{
			name:      "closed watch cannot prove repair ordering",
			wantError: "watch closed",
		},
		{
			name: "watch error cannot prove repair ordering",
			events: []watch.Event{
				{Type: watch.Error, Object: &metav1.Status{Status: "Failure", Message: "expired", Code: 410}},
			},
			wantError: "watch failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := watch.NewRaceFreeFake()
			defer stream.Stop()
			for _, event := range tc.events {
				stream.Action(event.Type, event.Object)
			}
			stream.Stop()
			node, err := waitForReplacement(context.Background(), stream, "original")
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
			} else {
				require.NoError(t, err)
				require.Equal(t, "replacement-node", node)
			}
		})
	}
}

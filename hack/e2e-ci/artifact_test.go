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

package e2eci

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestArtifactReceiptUsesExactServerIdentity(t *testing.T) {
	f := frozenFixture(t)
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"valid", nil},
		{"wrong id", func(a map[string]any) { a["id"] = 124 }},
		{"wrong name", func(a map[string]any) { a["name"] = "latest" }},
		{"wrong digest", func(a map[string]any) { a["digest"] = "sha256:" + strings.Repeat("b", 64) }},
		{"expired", func(a map[string]any) { a["expired"] = true }},
		{"oversize", func(a map[string]any) { a["size_in_bytes"] = MaxBundleBytes + 1 }},
		{"wrong run", func(a map[string]any) { a["workflow_run"].(map[string]any)["id"] = 43 }},
		{"fork run", func(a map[string]any) { a["workflow_run"].(map[string]any)["repository_id"] = 99 }},
		{"wrong sha", func(a map[string]any) { a["workflow_run"].(map[string]any)["head_sha"] = headSHA }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/"+Repository+"/actions/artifacts/123" {
					t.Errorf("artifact selection must use exact ID, got %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				a := map[string]any{"id": 123, "name": ArtifactName(f), "digest": digest, "size_in_bytes": 512, "expired": false, "workflow_run": map[string]any{"id": 42, "repository_id": RepositoryID, "head_repository_id": RepositoryID, "head_branch": "main", "head_sha": toolSHA}}
				if tc.change != nil {
					tc.change(a)
				}
				json.NewEncoder(w).Encode(a)
			}))
			defer server.Close()
			receipt, err := (Gate{Client: server.Client(), APIURL: server.URL}).Artifact(context.Background(), f, 123, digest)
			if tc.change != nil {
				if err == nil {
					t.Fatal("untrusted artifact accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := receipt.Validate(f); err != nil {
				t.Fatal(err)
			}
		})
	}
}

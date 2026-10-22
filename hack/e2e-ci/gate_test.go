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
	"testing"
)

func TestCaptureRejectsReadOnlyActor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + Repository:
			json.NewEncoder(w).Encode(repository{ID: RepositoryID, FullName: Repository, Owner: owner{ID: OwnerID}, DefaultBranch: "main"})
		case "/repos/" + Repository + "/actions/runs/42":
			json.NewEncoder(w).Encode(workflowRun{ID: 42, RunAttempt: 1, Event: "workflow_dispatch", Path: WorkflowPath, HeadBranch: "main", HeadSHA: toolSHA, Actor: actor{ID: 7, Login: "maintainer"}, Repository: repository{ID: RepositoryID, FullName: Repository}})
		case "/repos/" + Repository + "/collaborators/maintainer/permission":
			json.NewEncoder(w).Encode(permissionResponse{Permission: "read", User: actor{ID: 7, Login: "maintainer"}})
		default:
			t.Errorf("unexpected API request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	_, err := (Gate{Client: server.Client(), APIURL: server.URL}).Capture(context.Background(), validDispatch(), []string{"standard"})
	if err == nil {
		t.Fatal("read-only actor authorized")
	}
}

const toolSHA = "1111111111111111111111111111111111111111"
const headSHA = "2222222222222222222222222222222222222222"

func validDispatch() Dispatch {
	return Dispatch{Repository: Repository, Ref: "refs/heads/main", WorkflowRef: Repository + "/" + WorkflowPath + "@refs/heads/main", WorkflowSHA: toolSHA, Event: "workflow_dispatch", Actor: "maintainer", ActorID: 7, RunID: 42, RunAttempt: 1, PR: "12", Preset: "standard"}
}

func gateFixture(t *testing.T, mutate func(string, map[string]any)) Gate {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var value map[string]any
		switch r.URL.Path {
		case "/repos/" + Repository:
			value = map[string]any{"id": RepositoryID, "full_name": Repository, "owner": map[string]any{"id": OwnerID}, "default_branch": "main"}
		case "/repos/" + Repository + "/actions/runs/42":
			value = map[string]any{"id": 42, "run_attempt": 1, "event": "workflow_dispatch", "path": WorkflowPath, "head_branch": "main", "head_sha": toolSHA, "actor": map[string]any{"id": 7, "login": "maintainer"}, "repository": map[string]any{"id": RepositoryID, "full_name": Repository}}
		case "/repos/" + Repository + "/collaborators/maintainer/permission":
			value = map[string]any{"permission": "write", "user": map[string]any{"id": 7, "login": "maintainer"}}
		case "/repos/" + Repository + "/pulls/12":
			value = map[string]any{"number": 12, "state": "open", "base": map[string]any{"ref": "main", "repo": map[string]any{"id": RepositoryID, "full_name": Repository}}, "head": map[string]any{"sha": headSHA, "repo": map[string]any{"id": 99, "full_name": "contributor/fork"}}}
		default:
			t.Errorf("unexpected API path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if mutate != nil {
			mutate(r.URL.Path, value)
		}
		if err := json.NewEncoder(w).Encode(value); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	return Gate{Client: server.Client(), APIURL: server.URL}
}

func TestCaptureFreezesForkHeadAndRechecksWithoutRefreshing(t *testing.T) {
	rechecking := false
	gate := gateFixture(t, func(path string, v map[string]any) {
		if rechecking && path == "/repos/"+Repository+"/pulls/12" {
			t.Error("recheck must not resolve mutable PR head")
		}
	})
	frozen, err := gate.Capture(context.Background(), validDispatch(), []string{"standard"})
	if err != nil {
		t.Fatal(err)
	}
	if frozen.HeadSHA != headSHA || frozen.HeadRepository != "contributor/fork" || frozen.HeadRepositoryID != 99 {
		t.Fatalf("incorrect fork capture: %+v", frozen)
	}
	rechecking = true
	if err := gate.Recheck(context.Background(), frozen, validDispatch(), []string{"standard"}); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureRejectsInvalidDispatchBeforeAPI(t *testing.T) {
	cases := map[string]func(*Dispatch){
		"repository": func(d *Dispatch) { d.Repository = "attacker/fork" }, "ref": func(d *Dispatch) { d.Ref = "refs/heads/feature" }, "workflow": func(d *Dispatch) { d.WorkflowRef = "attacker/fork/" + WorkflowPath + "@refs/heads/main" }, "event": func(d *Dispatch) { d.Event = "issue_comment" }, "sha": func(d *Dispatch) { d.WorkflowSHA = "main" }, "rerun": func(d *Dispatch) { d.RunAttempt = 2 }, "actor": func(d *Dispatch) { d.Actor = "../x" }, "actor id": func(d *Dispatch) { d.ActorID = 0 }, "run": func(d *Dispatch) { d.RunID = 0 }, "negative pr": func(d *Dispatch) { d.PR = "-1" }, "zero pr": func(d *Dispatch) { d.PR = "0" }, "overflow pr": func(d *Dispatch) { d.PR = "999999999999999999999999" }, "shell pr": func(d *Dispatch) { d.PR = "12;echo x" }, "preset": func(d *Dispatch) { d.Preset = "all" }, "disabled preset": func(d *Dispatch) { d.Preset = "gpu" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			d := validDispatch()
			change(&d)
			gate := Gate{APIURL: "http://127.0.0.1:1"}
			if _, err := gate.Capture(context.Background(), d, []string{"standard"}); err == nil {
				t.Fatal("invalid dispatch accepted")
			}
		})
	}
}

func TestCaptureRejectsUntrustedGitHubMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, path, key string
		value           any
	}{
		{"repository id", "", "id", 1}, {"repository owner", "", "owner", map[string]any{"id": 1}}, {"default branch", "", "default_branch", "other"},
		{"run attempt", "/actions/runs/42", "run_attempt", 2}, {"run event", "/actions/runs/42", "event", "pull_request"}, {"run sha", "/actions/runs/42", "head_sha", headSHA}, {"run workflow", "/actions/runs/42", "path", "other.yml"}, {"actor id", "/actions/runs/42", "actor", map[string]any{"id": 8, "login": "maintainer"}},
		{"revoked permission", "/collaborators/maintainer/permission", "permission", "read"}, {"permission user", "/collaborators/maintainer/permission", "user", map[string]any{"id": 8, "login": "maintainer"}},
		{"closed PR", "/pulls/12", "state", "closed"}, {"wrong base", "/pulls/12", "base", map[string]any{"ref": "main", "repo": map[string]any{"id": 1, "full_name": Repository}}}, {"deleted fork", "/pulls/12", "head", map[string]any{"sha": headSHA, "repo": nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := gateFixture(t, func(path string, v map[string]any) {
				if path == "/repos/"+Repository+tc.path {
					v[tc.key] = tc.value
				}
			})
			if _, err := gate.Capture(context.Background(), validDispatch(), []string{"standard"}); err == nil {
				t.Fatal("untrusted metadata accepted")
			}
		})
	}
}

func TestRecheckRejectsRevocationAndChangedInvocation(t *testing.T) {
	revoked := false
	gate := gateFixture(t, func(path string, v map[string]any) {
		if revoked && path == "/repos/"+Repository+"/collaborators/maintainer/permission" {
			v["permission"] = "read"
		}
	})
	f, err := gate.Capture(context.Background(), validDispatch(), []string{"standard"})
	if err != nil {
		t.Fatal(err)
	}
	revoked = true
	if err := gate.Recheck(context.Background(), f, validDispatch(), []string{"standard"}); err == nil {
		t.Fatal("revoked permission accepted")
	}
	d := validDispatch()
	d.RunID++
	if err := gate.Recheck(context.Background(), f, d, []string{"standard"}); err == nil {
		t.Fatal("different run accepted")
	}
}

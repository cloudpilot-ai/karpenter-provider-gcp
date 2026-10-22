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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPendingCheckUsesFrozenForkSHA(t *testing.T) {
	f := Frozen{Schema: 1, Dispatch: validDispatch(), BaseRepositoryID: RepositoryID, PRNumber: 12, HeadRepositoryID: 99, HeadRepository: "contributor/fork", HeadSHA: headSHA}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/repos/"+Repository+"/check-runs" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["head_sha"] != headSHA || body["status"] != "in_progress" || body["external_id"] != "e2e:42:1:standard:pr:12" {
			t.Errorf("wrong check: %#v", body)
		}
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":123}`))
	}))
	defer server.Close()
	id, err := (Gate{APIURL: server.URL, Client: server.Client()}).Pending(context.Background(), f)
	if err != nil || id != 123 {
		t.Fatalf("pending %d %v", id, err)
	}
}

func TestPublicationPreservesUserCommentsAndRejectsStaleInvocation(t *testing.T) {
	f := Frozen{Schema: 1, Dispatch: validDispatch(), BaseRepositoryID: RepositoryID, PRNumber: 12, HeadRepositoryID: 99, HeadRepository: "contributor/fork", HeadSHA: headSHA}
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			if r.Method == "PATCH" && strings.HasSuffix(r.URL.Path, "/check-runs/123") {
				_, _ = w.Write([]byte(`{}`))
				return
			}
			writes++
			t.Errorf("stale publication overwrote shared comment %s", r.URL.Path)
		}
		if strings.Contains(r.URL.Path, "/commits/") {
			_, _ = w.Write([]byte(`{"check_runs":[]}`))
			return
		}
		switch r.URL.Path {
		case "/repos/" + Repository + "/check-runs/123":
			fmt.Fprintf(w, `{"id":123,"head_sha":%q,"external_id":%q,"app":{"slug":"github-actions"}}`, headSHA, externalID(f))
		case "/repos/" + Repository + "/pulls/12":
			fmt.Fprintf(w, `{"head":{"sha":%q}}`, headSHA)
		case "/repos/" + Repository + "/issues/12/comments":
			fmt.Fprintf(w, `[{"id":1,"user":{"login":"github-actions[bot]","type":"User"},"body":%q},{"id":2,"user":{"login":"github-actions[bot]","type":"Bot"},"body":%q}]`, commentMarker(f), strings.Replace(commentMarker(f), "run=42", "run=43", 1))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	result := (Gate{Client: server.Client(), APIURL: server.URL}).Publish(context.Background(), f, 123, "failure", "trusted rendered report")
	if writes != 0 || result.Comment != "superseded" || result.Check != "passed" {
		t.Fatalf("%+v writes=%d", result, writes)
	}
}

func TestSupersededInvocationStillFinalizesItsOwnCheck(t *testing.T) {
	f := Frozen{Schema: 1, Dispatch: validDispatch(), BaseRepositoryID: RepositoryID, PRNumber: 12, HeadRepositoryID: 99, HeadRepository: "contributor/fork", HeadSHA: headSHA}
	patched := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/commits/") {
			_, _ = w.Write([]byte(`{"check_runs":[]}`))
			return
		}
		switch {
		case r.URL.Path == "/repos/"+Repository+"/check-runs/123" && r.Method == "GET":
			fmt.Fprintf(w, `{"id":123,"head_sha":%q,"external_id":%q,"app":{"slug":"github-actions"}}`, headSHA, externalID(f))
		case r.URL.Path == "/repos/"+Repository+"/check-runs/123" && r.Method == "PATCH":
			patched = true
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprintf(w, `[{"id":2,"user":{"login":"github-actions[bot]","type":"Bot"},"body":%q}]`, strings.Replace(commentMarker(f), "run=42", "run=43", 1))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	result := (Gate{Client: server.Client(), APIURL: server.URL}).Publish(context.Background(), f, 123, "failure", "trusted report")
	if !patched || result.Check != "passed" || result.Comment != "superseded" {
		t.Fatalf("older check left pending: %+v patched=%v", result, patched)
	}
}

func TestNewerCheckPreventsOverwriteWhenItsCommentFailed(t *testing.T) {
	f := Frozen{Schema: 1, Dispatch: validDispatch(), BaseRepositoryID: RepositoryID, PRNumber: 12, HeadRepositoryID: 99, HeadRepository: "contributor/fork", HeadSHA: headSHA}
	commentWrites := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/commits/"):
			fmt.Fprintf(w, `{"check_runs":[{"head_sha":%q,"external_id":"e2e:43:1:standard:pr:12","app":{"slug":"github-actions"}}]}`, headSHA)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/check-runs/123"):
			fmt.Fprintf(w, `{"id":123,"head_sha":%q,"external_id":%q,"app":{"slug":"github-actions"}}`, headSHA, externalID(f))
		case r.Method == "PATCH" && strings.Contains(r.URL.Path, "/check-runs/"):
			_, _ = w.Write([]byte(`{}`))
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/comments"):
			_, _ = w.Write([]byte(`[]`))
		case strings.HasSuffix(r.URL.Path, "/pulls/12"):
			fmt.Fprintf(w, `{"head":{"sha":%q}}`, headSHA)
		case r.Method == "POST":
			commentWrites++
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	result := (Gate{Client: server.Client(), APIURL: server.URL}).Publish(context.Background(), f, 123, "failure", "older results")
	if commentWrites != 0 || result.Comment != "superseded" {
		t.Fatalf("newer failed publication lost: %+v writes=%d", result, commentWrites)
	}
}

func TestPublicationReadFailuresStillFinalizeCheck(t *testing.T) {
	for _, failure := range []string{"comments", "checks"} {
		t.Run(failure, func(t *testing.T) {
			f := Frozen{Schema: 1, Dispatch: validDispatch(), BaseRepositoryID: RepositoryID, PRNumber: 12, HeadRepositoryID: 99, HeadRepository: "contributor/fork", HeadSHA: headSHA}
			finalized := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/check-runs/123"):
					fmt.Fprintf(w, `{"id":123,"head_sha":%q,"external_id":%q,"app":{"slug":"github-actions"}}`, headSHA, externalID(f))
				case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/check-runs/123"):
					var body struct{ Status, Conclusion string }
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					finalized = body.Status == "completed" && body.Conclusion == "failure"
					_, _ = w.Write([]byte(`{}`))
				case strings.HasSuffix(r.URL.Path, "/comments") && r.Method == http.MethodGet:
					if failure == "comments" {
						w.WriteHeader(500)
					} else {
						_, _ = w.Write([]byte(`[]`))
					}
				case strings.Contains(r.URL.Path, "/commits/"):
					w.WriteHeader(500)
				default:
					t.Errorf("unsafe/unexpected request %s %s", r.Method, r.URL.Path)
				}
			}))
			defer server.Close()
			result := (Gate{Client: server.Client(), APIURL: server.URL}).Publish(context.Background(), f, 123, "failure", "trusted report")
			if !finalized || result.Check != "passed" || result.Comment != "failed" {
				t.Fatalf("check finalization depends on comment reads: %+v finalized=%v", result, finalized)
			}
		})
	}
}

func TestOtherPRWithSameSHADoesNotSupersedeComment(t *testing.T) {
	f := Frozen{Schema: 1, Dispatch: validDispatch(), BaseRepositoryID: RepositoryID, PRNumber: 12, HeadRepositoryID: 99, HeadRepository: "contributor/fork", HeadSHA: headSHA}
	other := f
	other.PRNumber, other.Dispatch.PR, other.Dispatch.RunID = 13, "13", 43
	commented := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/commits/"):
			fmt.Fprintf(w, `{"check_runs":[{"head_sha":%q,"external_id":%q,"app":{"slug":"github-actions"}}]}`, headSHA, externalID(other))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/check-runs/123"):
			fmt.Fprintf(w, `{"id":123,"head_sha":%q,"external_id":%q,"app":{"slug":"github-actions"}}`, headSHA, externalID(f))
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/check-runs/"):
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			_, _ = w.Write([]byte(`[]`))
		case strings.HasSuffix(r.URL.Path, "/pulls/12"):
			fmt.Fprintf(w, `{"head":{"sha":%q}}`, headSHA)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues/12/comments"):
			commented = true
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	result := (Gate{Client: server.Client(), APIURL: server.URL}).Publish(context.Background(), f, 123, "failure", "trusted report")
	if !commented || result.Check != "passed" || result.Comment != "passed" {
		t.Fatalf("another PR suppressed this PR's comment: %+v", result)
	}
}

func TestPublicationFailuresAndStaleSHA(t *testing.T) {
	for _, tc := range []struct {
		name                                                string
		checkFailure, commentFailure, staleSHA, userComment bool
		wantCheck, wantComment                              string
	}{
		{name: "check failure still attempts comment", checkFailure: true, wantCheck: "failed", wantComment: "passed"},
		{name: "comment failure preserves finalized check", commentFailure: true, wantCheck: "passed", wantComment: "failed"},
		{name: "PR moved after gate", staleSHA: true, wantCheck: "passed", wantComment: "stale-sha"},
		{name: "user marker preserved", userComment: true, wantCheck: "passed", wantComment: "passed"},
		{name: "bot comment updated", wantCheck: "passed", wantComment: "passed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := Frozen{Schema: 1, Dispatch: validDispatch(), BaseRepositoryID: RepositoryID, PRNumber: 12, HeadRepositoryID: 99, HeadRepository: "contributor/fork", HeadSHA: headSHA}
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/commits/"):
					_, _ = w.Write([]byte(`{"check_runs":[]}`))
				case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/check-runs/123"):
					fmt.Fprintf(w, `{"id":123,"head_sha":%q,"external_id":%q,"app":{"slug":"github-actions"}}`, headSHA, externalID(f))
				case r.Method == "PATCH" && strings.HasSuffix(r.URL.Path, "/check-runs/123"):
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if body["conclusion"] != "cancelled" {
						t.Errorf("lost cancellation: %v", body)
					}
					if tc.checkFailure {
						w.WriteHeader(403)
					} else {
						_, _ = w.Write([]byte(`{}`))
					}
				case strings.HasSuffix(r.URL.Path, "/pulls/12"):
					sha := headSHA
					if tc.staleSHA {
						sha = toolSHA
					}
					fmt.Fprintf(w, `{"head":{"sha":%q}}`, sha)
				case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/comments"):
					kind := "Bot"
					if tc.userComment {
						kind = "User"
					}
					fmt.Fprintf(w, `[{"id":555,"user":{"login":"github-actions[bot]","type":%q},"body":%q}]`, kind, commentMarker(f))
				case r.Method == "POST" || r.Method == "PATCH":
					writes++
					if tc.userComment && r.Method != "POST" {
						t.Error("modified user comment")
					}
					if !tc.userComment && (r.Method != "PATCH" || !strings.HasSuffix(r.URL.Path, "/issues/comments/555")) {
						t.Error("did not update bot comment")
					}
					if tc.commentFailure {
						w.WriteHeader(500)
					} else {
						_, _ = w.Write([]byte(`{}`))
					}
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			result := (Gate{APIURL: server.URL, Client: server.Client()}).Publish(context.Background(), f, 123, "cancelled", "trusted report")
			if result.Check != tc.wantCheck || result.Comment != tc.wantComment {
				t.Fatalf("%+v", result)
			}
			if tc.staleSHA && writes != 0 {
				t.Fatal("stale SHA posted comment")
			}
		})
	}
}

func TestPublisherRejectsUnrelatedCheckAndBounds(t *testing.T) {
	for _, tc := range []struct{ name, sha, app, external, markdown string }{
		{"wrong SHA", toolSHA, "github-actions", "e2e:42:1:standard:pr:12", "trusted"},
		{"wrong app", headSHA, "other", "e2e:42:1:standard:pr:12", "trusted"},
		{"wrong invocation", headSHA, "github-actions", "e2e:41:1:standard:pr:12", "trusted"},
		{"wrong PR", headSHA, "github-actions", "e2e:42:1:standard:pr:13", "trusted"},
		{"oversize", headSHA, "github-actions", "e2e:42:1:standard:pr:12", strings.Repeat("x", 60001)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := Frozen{Schema: 1, Dispatch: validDispatch(), BaseRepositoryID: RepositoryID, PRNumber: 12, HeadRepositoryID: 99, HeadRepository: "contributor/fork", HeadSHA: headSHA}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Error("wrote unrelated check")
				}
				fmt.Fprintf(w, `{"id":123,"head_sha":%q,"external_id":%q,"app":{"slug":%q}}`, tc.sha, tc.external, tc.app)
			}))
			defer server.Close()
			result := (Gate{Client: server.Client(), APIURL: server.URL}).Publish(context.Background(), f, 123, "success", tc.markdown)
			if result.Check != "failed" {
				t.Fatalf("accepted %+v", result)
			}
		})
	}
}
func TestPendingCheckPlatformFailureAndCancellation(t *testing.T) {
	f := Frozen{Schema: 1, Dispatch: validDispatch(), BaseRepositoryID: RepositoryID, PRNumber: 12, HeadRepositoryID: 99, HeadRepository: "contributor/fork", HeadSHA: headSHA}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(422) }))
	defer server.Close()
	g := Gate{Client: server.Client(), APIURL: server.URL}
	if _, err := g.Pending(context.Background(), f); err == nil {
		t.Fatal("fork-check platform error ignored")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.Pending(ctx, f); err == nil {
		t.Fatal("cancelled request passed")
	}
}

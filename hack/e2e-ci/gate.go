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

// Package e2eci defines the trusted, credential-free handoff for manually dispatched e2e runs.
package e2eci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const (
	Repository         = "cloudpilot-ai/karpenter-provider-gcp"
	RepositoryID int64 = 863038341
	OwnerID      int64 = 166281999
	WorkflowPath       = ".github/workflows/e2e.yaml"
)

var shaPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
var positivePattern = regexp.MustCompile(`^[1-9][0-9]*$`)
var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var loginPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)

// Dispatch is populated from the trusted Actions context, not from PR artifacts.
type Dispatch struct {
	Repository  string `json:"repository"`
	Ref         string `json:"ref"`
	WorkflowRef string `json:"workflow_ref"`
	WorkflowSHA string `json:"workflow_sha"`
	Event       string `json:"event"`
	Actor       string `json:"actor"`
	ActorID     int64  `json:"actor_id"`
	RunID       int64  `json:"run_id"`
	RunAttempt  int    `json:"run_attempt"`
	PR          string `json:"pr"`
	Preset      string `json:"preset"`
}

// Frozen is trusted gate output. It contains no runtime target configuration.
// Preserve it through job outputs independently of the untrusted build artifact.
type Frozen struct {
	Schema           int      `json:"schema"`
	Dispatch         Dispatch `json:"dispatch"`
	BaseRepositoryID int64    `json:"base_repository_id"`
	PRNumber         int64    `json:"pr_number"`
	HeadRepositoryID int64    `json:"head_repository_id"`
	HeadRepository   string   `json:"head_repository"`
	HeadSHA          string   `json:"head_sha"`
}

func (d Dispatch) validate(enabled []string) error {
	if d.Repository != Repository || d.Ref != "refs/heads/main" || d.WorkflowRef != Repository+"/"+WorkflowPath+"@refs/heads/main" || d.Event != "workflow_dispatch" || !shaPattern.MatchString(d.WorkflowSHA) {
		return errors.New("dispatch must use the upstream main manual workflow")
	}
	if d.RunAttempt != 1 || d.RunID <= 0 || d.ActorID <= 0 || !loginPattern.MatchString(d.Actor) {
		return errors.New("invalid invocation or actor; reruns require a new dispatch")
	}
	if !positivePattern.MatchString(d.PR) {
		return errors.New("PR must be a positive integer")
	}
	if _, err := strconv.ParseInt(d.PR, 10, 64); err != nil {
		return errors.New("PR out of range")
	}
	if d.Preset != "standard" && d.Preset != "gpu" && d.Preset != "full" {
		return errors.New("unknown preset")
	}
	for _, preset := range enabled {
		if preset == d.Preset {
			return nil
		}
	}
	return errors.New("preset is not administratively enabled")
}
func (f Frozen) Validate() error {
	if err := f.Dispatch.validate([]string{f.Dispatch.Preset}); err != nil {
		return err
	}
	pr, _ := strconv.ParseInt(f.Dispatch.PR, 10, 64)
	if f.Schema != 1 || f.BaseRepositoryID != RepositoryID || f.PRNumber != pr || f.HeadRepositoryID <= 0 || !repositoryPattern.MatchString(f.HeadRepository) || !shaPattern.MatchString(f.HeadSHA) {
		return errors.New("invalid frozen source")
	}
	return nil
}

type owner struct {
	ID int64 `json:"id"`
}
type repository struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	Owner         owner  `json:"owner"`
	DefaultBranch string `json:"default_branch"`
}
type actor struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}
type workflowRun struct {
	ID         int64      `json:"id"`
	RunAttempt int        `json:"run_attempt"`
	Event      string     `json:"event"`
	Path       string     `json:"path"`
	HeadBranch string     `json:"head_branch"`
	HeadSHA    string     `json:"head_sha"`
	Actor      actor      `json:"actor"`
	Repository repository `json:"repository"`
}
type permissionResponse struct {
	Permission string `json:"permission"`
	User       actor  `json:"user"`
}
type pullRequest struct {
	Number int64  `json:"number"`
	State  string `json:"state"`
	Base   struct {
		Ref  string     `json:"ref"`
		Repo repository `json:"repo"`
	} `json:"base"`
	Head struct {
		SHA  string     `json:"sha"`
		Repo repository `json:"repo"`
	} `json:"head"`
}

// Gate talks only to GitHub. APIURL is overridable for boundary tests; the CLI fixes it to api.github.com.
type Gate struct {
	Client *http.Client
	Token  string
	APIURL string
}

func (g Gate) get(ctx context.Context, path string, dest any) error {
	base := g.APIURL
	if base == "" {
		base = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	client := g.Client
	if client == nil {
		client = http.DefaultClient
	}
	// Do not forward authorization across redirects.
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := copyClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub %s returned %d", path, res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("GitHub response too large")
	}
	return json.Unmarshal(data, dest)
}
func (g Gate) authorize(ctx context.Context, d Dispatch, enabled []string) error {
	if err := d.validate(enabled); err != nil {
		return err
	}
	var repo repository
	if err := g.get(ctx, "/repos/"+Repository, &repo); err != nil {
		return err
	}
	if repo.ID != RepositoryID || repo.FullName != Repository || repo.Owner.ID != OwnerID || repo.DefaultBranch != "main" {
		return errors.New("upstream repository identity/default branch mismatch")
	}
	var run workflowRun
	if err := g.get(ctx, fmt.Sprintf("/repos/%s/actions/runs/%d", Repository, d.RunID), &run); err != nil {
		return err
	}
	if run.ID != d.RunID || run.RunAttempt != 1 || run.Event != d.Event || run.Path != WorkflowPath || run.HeadBranch != "main" || run.HeadSHA != d.WorkflowSHA || run.Actor.ID != d.ActorID || run.Actor.Login != d.Actor || run.Repository.ID != RepositoryID || run.Repository.FullName != Repository {
		return errors.New("Actions run does not match trusted dispatch")
	}
	var permission permissionResponse
	if err := g.get(ctx, "/repos/"+Repository+"/collaborators/"+url.PathEscape(d.Actor)+"/permission", &permission); err != nil {
		return err
	}
	if permission.User.ID != d.ActorID || permission.User.Login != d.Actor || (permission.Permission != "write" && permission.Permission != "maintain" && permission.Permission != "admin") {
		return errors.New("actor no longer has write/maintain/admin permission")
	}
	return nil
}

func (g Gate) Capture(ctx context.Context, d Dispatch, enabled []string) (Frozen, error) {
	if err := g.authorize(ctx, d, enabled); err != nil {
		return Frozen{}, err
	}
	var pr pullRequest
	if err := g.get(ctx, "/repos/"+Repository+"/pulls/"+d.PR, &pr); err != nil {
		return Frozen{}, err
	}
	number, _ := strconv.ParseInt(d.PR, 10, 64)
	if pr.Number != number || pr.State != "open" || pr.Base.Ref != "main" || pr.Base.Repo.ID != RepositoryID || pr.Base.Repo.FullName != Repository {
		return Frozen{}, errors.New("PR must be open against upstream main")
	}
	frozen := Frozen{Schema: 1, Dispatch: d, BaseRepositoryID: RepositoryID, PRNumber: number, HeadRepositoryID: pr.Head.Repo.ID, HeadRepository: pr.Head.Repo.FullName, HeadSHA: pr.Head.SHA}
	return frozen, frozen.Validate()
}

// Recheck requires the exact current trusted context and never queries or refreshes the PR head.
func (g Gate) Recheck(ctx context.Context, f Frozen, current Dispatch, enabled []string) error {
	if err := f.Validate(); err != nil {
		return err
	}
	if current != f.Dispatch {
		return errors.New("frozen dispatch does not match current invocation")
	}
	return g.authorize(ctx, current, enabled)
}

// DecodeStrict bounds trusted metadata files too; accidental extra fields fail closed.
func DecodeStrict(r io.Reader, limit int64, v any) error {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return errors.New("metadata too large")
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return errors.New("trailing metadata")
	}
	return nil
}

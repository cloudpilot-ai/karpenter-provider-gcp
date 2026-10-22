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
	"errors"
	"fmt"
)

// Receipt is trusted upload metadata, not a manifest field. Obtain ID/digest from
// this invocation's upload outputs, then confirm them with the GitHub API.
type Receipt struct {
	Schema      int    `json:"schema"`
	ID          int64  `json:"id"`
	Digest      string `json:"digest"`
	RunID       int64  `json:"run_id"`
	RunAttempt  int    `json:"run_attempt"`
	WorkflowSHA string `json:"workflow_sha"`
	Name        string `json:"name"`
}

func ArtifactName(f Frozen) string {
	return fmt.Sprintf("e2e-bundle-%d-%d", f.Dispatch.RunID, f.Dispatch.RunAttempt)
}
func (r Receipt) Validate(f Frozen) error {
	if err := f.Validate(); err != nil {
		return err
	}
	if r.Schema != 1 || r.ID <= 0 || !digestPattern.MatchString(r.Digest) || r.RunID != f.Dispatch.RunID || r.RunAttempt != f.Dispatch.RunAttempt || r.WorkflowSHA != f.Dispatch.WorkflowSHA || r.Name != ArtifactName(f) {
		return errors.New("artifact does not match invocation")
	}
	return nil
}
func (g Gate) Artifact(ctx context.Context, f Frozen, id int64, digest string) (Receipt, error) {
	r, err := g.confirmArtifact(ctx, f, id, digest, ArtifactName(f))
	if err != nil {
		return Receipt{}, err
	}
	return r, r.Validate(f)
}

func ReportArtifactName(f Frozen) string {
	return fmt.Sprintf("e2e-results-%d-%d", f.Dispatch.RunID, f.Dispatch.RunAttempt)
}

// ConfirmReport requires this invocation's upload outputs, not a latest-name lookup.
func (g Gate) ConfirmReport(ctx context.Context, f Frozen, id int64, digest string) error {
	_, err := g.confirmArtifact(ctx, f, id, digest, ReportArtifactName(f))
	return err
}
func (g Gate) confirmArtifact(ctx context.Context, f Frozen, id int64, digest, name string) (Receipt, error) {
	if err := f.Validate(); err != nil {
		return Receipt{}, err
	}
	if id <= 0 || !digestPattern.MatchString(digest) {
		return Receipt{}, errors.New("invalid artifact ID/digest")
	}
	var artifact struct {
		ID      int64  `json:"id"`
		Name    string `json:"name"`
		Digest  string `json:"digest"`
		Expired bool   `json:"expired"`
		Size    int64  `json:"size_in_bytes"`
		Run     struct {
			ID               int64  `json:"id"`
			RepositoryID     int64  `json:"repository_id"`
			HeadRepositoryID int64  `json:"head_repository_id"`
			HeadSHA          string `json:"head_sha"`
			HeadBranch       string `json:"head_branch"`
		} `json:"workflow_run"`
	}
	if err := g.get(ctx, fmt.Sprintf("/repos/%s/actions/artifacts/%d", Repository, id), &artifact); err != nil {
		return Receipt{}, err
	}
	if artifact.ID != id || artifact.Digest != digest || artifact.Name != name || artifact.Expired || artifact.Size <= 0 || artifact.Size > MaxBundleBytes || artifact.Run.ID != f.Dispatch.RunID || artifact.Run.RepositoryID != RepositoryID || artifact.Run.HeadRepositoryID != RepositoryID || artifact.Run.HeadSHA != f.Dispatch.WorkflowSHA || artifact.Run.HeadBranch != "main" {
		return Receipt{}, errors.New("GitHub artifact provenance mismatch")
	}
	receipt := Receipt{Schema: 1, ID: id, Digest: digest, Name: artifact.Name, RunID: f.Dispatch.RunID, RunAttempt: f.Dispatch.RunAttempt, WorkflowSHA: f.Dispatch.WorkflowSHA}
	return receipt, nil
}

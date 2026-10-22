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

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2/types"

	e2eci "github.com/cloudpilot-ai/karpenter-provider-gcp/hack/e2e-ci"
)

func exportSource() e2eci.Frozen {
	return e2eci.Frozen{Schema: 1, BaseRepositoryID: e2eci.RepositoryID, PRNumber: 7, HeadRepositoryID: 3, HeadRepository: "contributor/fork", HeadSHA: strings.Repeat("a", 40), Dispatch: e2eci.Dispatch{Repository: e2eci.Repository, Ref: "refs/heads/main", WorkflowRef: e2eci.Repository + "/" + e2eci.WorkflowPath + "@refs/heads/main", WorkflowSHA: strings.Repeat("b", 40), Event: "workflow_dispatch", Actor: "maintainer", ActorID: 1, RunID: 9, RunAttempt: 1, PR: "7", Preset: "standard"}}
}
func TestPublicExportRemovesArbitraryNativeText(t *testing.T) {
	source := exportSource()
	dir := t.TempDir()
	out := filepath.Join(t.TempDir(), "public")
	secret := "ya29.synthetic-credential\n<script>**token**|\x1b"
	writeNativeFixture(t, dir, types.Report{SuiteSucceeded: true, SuitePath: secret, SuiteDescription: secret, SpecReports: types.SpecReports{{LeafNodeType: types.NodeTypeIt, State: types.SpecStatePassed, LeafNodeText: secret, CapturedGinkgoWriterOutput: secret, LeafNodeLabels: []string{"suite:provisioning", secret}}}})
	o := runOutcome{Schema: 1, Source: source, Requested: "standard", Resolved: "standard", Workers: 20, Started: time.Now(), Finished: time.Now(), Image: "example.invalid/controller@sha256:" + strings.Repeat("c", 64), ControllerCommit: source.HeadSHA, Infrastructure: statePassed, Tests: statePassed, Reports: statePassed, Cleanup: statePassed, Diagnostics: statePassed, Artifact: stateNotRun, Publication: stateNotRun, Counts: specCounts{Executed: 1, Passed: 1}}
	if err := persistOutcome(dir, o); err != nil {
		t.Fatal(err)
	}
	if err := executePublicExport(t, dir, out, source); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(out)
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(out, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"ya29.", "synthetic-credential", "<script>", "token", "example.invalid"} {
			if bytes.Contains(data, []byte(bad)) {
				t.Fatalf("%s leaked %s", entry.Name(), bad)
			}
		}
	}
	data, _ := os.ReadFile(filepath.Join(out, "outcome.json"))
	var got runOutcome
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Overall() != statePassed {
		t.Fatalf("projection lost outcome: %+v", got)
	}
	reports, counts, err := readNativeReports(out)
	if err != nil || counts.Executed != 1 || reports[0].SpecReports[0].FullText() != "spec-00001" {
		t.Fatalf("bad native projection %v %+v", err, counts)
	}
}

func TestPublicExportRetainsOnlyNumericControllerFindings(t *testing.T) {
	source := exportSource()
	dir := t.TempDir()
	out := filepath.Join(t.TempDir(), "public")
	o := runOutcome{Schema: 1, Source: source, Requested: "standard", Resolved: "standard", Workers: 20, Started: time.Now(), Finished: time.Now(), Infrastructure: stateFailed, Tests: stateNotRun, Reports: stateUnavailable, Cleanup: stateNotRun, Diagnostics: statePassed, Artifact: stateNotRun, Publication: stateNotRun}
	if err := persistOutcome(dir, o); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "controller.log"), []byte("{\"severity\":\"ERROR\",\"message\":\"ya29.private-token\"}\n{\"level\":\"warn\",\"message\":\"secret\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := executePublicExport(t, dir, out, source); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(out, "controller.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"errors":1,"warnings":1`)) || bytes.Contains(data, []byte("ya29")) {
		t.Fatalf("unsafe/missing findings: %s", data)
	}
}

func TestControllerLogCaptureFormats(t *testing.T) {
	for _, prefix := range []string{"", "2026-05-09T12:00:00Z ", "[pod/karpenter-123/karpenter] 2026-05-09T12:00:00.123456789Z "} {
		t.Run(prefix, func(t *testing.T) {
			dir := t.TempDir()
			data := prefix + `{"level":"ERROR","message":"private"}` + "\n" + prefix + `{"severity":"WARNING","message":"private"}` + "\n"
			if err := os.WriteFile(filepath.Join(dir, "controller.log"), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			got := publicLogFindings(dir)
			if !got.Available || got.Errors != 1 || got.Warnings != 1 || got.Parsed != 2 || got.Omitted != 0 {
				t.Fatalf("captured controller JSON not recognized: %+v", got)
			}
		})
	}
}

func executePublicExport(t *testing.T, dir, out string, source e2eci.Frozen) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gate.json")
	data, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	c := newRunnerCommand()
	c.SetArgs([]string{"export", "--source", path, "--results-dir", dir, "--output", out})
	c.SetOut(&bytes.Buffer{})
	c.SetErr(&bytes.Buffer{})
	return c.Execute()
}
func TestPublicExportCancellationAndMissingEvidenceCannotPass(t *testing.T) {
	for _, tc := range []struct {
		name      string
		state     outcomeState
		wantError bool
	}{
		{"cancelled without native evidence", stateCancelled, false}, {"missing claimed successful native evidence", statePassed, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := exportSource()
			dir := t.TempDir()
			out := filepath.Join(t.TempDir(), "public")
			o := runOutcome{Schema: 1, Source: source, Requested: "standard", Resolved: "standard", Workers: 20, Started: time.Now(), Finished: time.Now(), Infrastructure: statePassed, Image: "example.invalid/controller@sha256:" + strings.Repeat("c", 64), ControllerCommit: source.HeadSHA, Tests: tc.state, Reports: stateUnavailable, Cleanup: statePassed, Diagnostics: stateUnavailable, Artifact: stateNotRun, Publication: stateNotRun}
			if tc.state == statePassed {
				o.Reports = statePassed
				o.Counts = specCounts{Executed: 1, Passed: 1}
			}
			if err := persistOutcome(dir, o); err != nil {
				t.Fatal(err)
			}
			err := executePublicExport(t, dir, out, source)
			if (err != nil) != tc.wantError {
				t.Fatalf("got %v", err)
			}
			if tc.wantError {
				if _, err := os.Stat(out); !os.IsNotExist(err) {
					t.Fatal("failed validation exported evidence")
				}
				return
			}
			data, err := os.ReadFile(filepath.Join(out, "outcome.json"))
			if err != nil {
				t.Fatal(err)
			}
			var got runOutcome
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if got.Overall() != stateCancelled {
				t.Fatalf("cancellation became %s", got.Overall())
			}
		})
	}
}
func TestPublicExportRejectsSymlinkedNativeEvidence(t *testing.T) {
	source := exportSource()
	dir := t.TempDir()
	out := filepath.Join(t.TempDir(), "public")
	o := runOutcome{Schema: 1, Source: source, Requested: "standard", Resolved: "standard", Workers: 20, Started: time.Now(), Finished: time.Now(), Infrastructure: statePassed, Image: "example.invalid/controller@sha256:" + strings.Repeat("c", 64), ControllerCommit: source.HeadSHA, Tests: statePassed, Reports: statePassed, Cleanup: statePassed, Diagnostics: stateUnavailable, Artifact: stateNotRun, Publication: stateNotRun, Counts: specCounts{Executed: 1, Passed: 1}}
	if err := persistOutcome(dir, o); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(elsewhere, []byte("synthetic-not-a-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(dir, "results.json")); err != nil {
		t.Fatal(err)
	}
	if err := executePublicExport(t, dir, out, source); err == nil {
		t.Fatal("linked report accepted")
	}
}

func TestPublicExportBoundsProjectedControllerFindings(t *testing.T) {
	source := exportSource()
	dir := t.TempDir()
	out := filepath.Join(t.TempDir(), "public")
	o := runOutcome{Schema: 1, Source: source, Requested: "standard", Resolved: "standard", Workers: 20, Started: time.Now(), Finished: time.Now(), Infrastructure: stateFailed, Tests: stateNotRun, Reports: stateUnavailable, Cleanup: stateNotRun, Diagnostics: statePassed, Artifact: stateNotRun, Publication: stateNotRun}
	if err := persistOutcome(dir, o); err != nil {
		t.Fatal(err)
	}
	hostile := `{"schema":1,"available":true,"errors":9223372036854775807,"warnings":9223372036854775807,"parsed":1,"omitted":9223372036854775807}`
	if err := os.WriteFile(filepath.Join(dir, "controller.log"), []byte(hostile), 0600); err != nil {
		t.Fatal(err)
	}
	if err := executePublicExport(t, dir, out, source); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(out, "controller.log"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("9223372036854775807")) {
		t.Fatalf("unbounded numeric evidence: %s", data)
	}
}

func TestPublicExportRejectsOverflowedCounts(t *testing.T) {
	source := exportSource()
	dir := t.TempDir()
	out := filepath.Join(t.TempDir(), "public")
	o := runOutcome{Schema: 1, Source: source, Requested: "standard", Resolved: "standard", Workers: 20, Started: time.Now(), Finished: time.Now(), Infrastructure: stateFailed, Tests: stateNotRun, Reports: stateUnavailable, Cleanup: stateNotRun, Diagnostics: stateUnavailable, Artifact: stateNotRun, Publication: stateNotRun, Counts: specCounts{Executed: int(^uint(0) >> 1), Passed: int(^uint(0) >> 1), Skipped: 1}}
	if err := persistOutcome(dir, o); err != nil {
		t.Fatal(err)
	}
	if err := executePublicExport(t, dir, out, source); err == nil {
		t.Fatal("overflowed counts accepted")
	}
}

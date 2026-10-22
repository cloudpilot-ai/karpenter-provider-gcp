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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2/types"

	e2eci "github.com/cloudpilot-ai/karpenter-provider-gcp/hack/e2e-ci"
)

func TestOfflineRenderUsesFrozenSourceWithoutExecutingTools(t *testing.T) {
	bundle, source, image := preparedFixture(t)
	fakeController(t, image, strings.Split(image, "@")[1], source.HeadSHA)
	dir := filepath.Join(t.TempDir(), "run")
	if _, err := executePrepared(context.Background(), preparedExecution{Bundle: bundle, Source: source, Image: image, Workers: 20, OutputDir: dir}); err != nil {
		t.Fatal(err)
	}
	gate := filepath.Join(t.TempDir(), "gate.json")
	data, _ := json.Marshal(source)
	if err := os.WriteFile(gate, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "report.md")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir()) // rendering must not need git, kubectl, go or PR binaries
	command := newRunnerCommand()
	command.SetArgs([]string{"render", "--source", gate, "--results-dir", dir})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	rendered, err := os.ReadFile(filepath.Join(dir, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), source.HeadSHA) || !strings.Contains(string(rendered), source.Dispatch.WorkflowSHA) || !strings.Contains(string(rendered), "incomplete") {
		t.Fatalf("missing frozen attribution/lifecycle: %s", rendered)
	}
	source.HeadSHA = strings.Repeat("d", 40)
	data, _ = json.Marshal(source)
	if err := os.WriteFile(gate, data, 0600); err != nil {
		t.Fatal(err)
	}
	command = newRunnerCommand()
	command.SetArgs([]string{"render", "--source", gate, "--results-dir", dir})
	if err := command.Execute(); err == nil {
		t.Fatal("render accepted different frozen SHA")
	}
}

func TestReportBoundsAndEscapesUntrustedText(t *testing.T) {
	hostile := "<script>**bad**|`code`\n# injected\x1b\u202e" + strings.Repeat("X", 100000)
	reports := []types.Report{{SuiteSucceeded: true, SuitePath: hostile, SpecReports: types.SpecReports{{LeafNodeType: types.NodeTypeIt, State: types.SpecStatePassed, LeafNodeText: hostile, LeafNodeLabels: []string{"suite:" + hostile}}}}}
	var output bytes.Buffer
	if err := renderReport(&output, reports, reportMetadata{Commit: hostile}); err != nil {
		t.Fatal(err)
	}
	if output.Len() > 60000 {
		t.Fatalf("unbounded public output: %d", output.Len())
	}
	for _, unsafe := range []string{"<script>", "**bad**", "`code`", "\n# injected", "\x1b", "\u202e"} {
		if strings.Contains(output.String(), unsafe) {
			t.Fatalf("unescaped %q", unsafe)
		}
	}
}

func TestLocalReportRejectsZeroTestsWithoutDeletingNative(t *testing.T) {
	dir := t.TempDir()
	writeNativeFixture(t, dir, types.Report{SuiteSucceeded: true, SpecReports: types.SpecReports{{LeafNodeType: types.NodeTypeIt, State: types.SpecStateSkipped}}})
	if err := writeReport(filepath.Join(dir, "results.json"), filepath.Join(dir, "report.md"), reportMetadata{}); err == nil {
		t.Fatal("local report claims success with no executed tests")
	}
	for _, name := range []string{"results.json", "junit.xml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOfflineRenderDoesNotLeaveStaleSuccessOnInvalidEvidence(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("stale or PR-supplied success"), 0600); err != nil {
		t.Fatal(err)
	}
	gate := filepath.Join(t.TempDir(), "gate.json")
	if err := os.WriteFile(gate, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	command := newRunnerCommand()
	command.SetArgs([]string{"render", "--source", gate, "--results-dir", dir})
	if err := command.Execute(); err == nil {
		t.Fatal("invalid evidence accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "report.md")); !os.IsNotExist(err) {
		t.Fatalf("stale success survived rendering failure: %v", err)
	}
}

func TestOfflineRenderRejectsContradictoryNativeSuite(t *testing.T) {
	_, source, image := preparedFixture(t)
	dir := t.TempDir()
	writeNativeFixture(t, dir, types.Report{SuiteSucceeded: false, SpecReports: types.SpecReports{{LeafNodeType: types.NodeTypeIt, State: types.SpecStatePassed, LeafNodeText: "passed before suite failure"}}})
	outcome := completedOutcome(source, image)
	if err := persistOutcome(dir, outcome); err != nil {
		t.Fatal(err)
	}
	gate := filepath.Join(t.TempDir(), "gate.json")
	data, _ := json.Marshal(source)
	if err := os.WriteFile(gate, data, 0600); err != nil {
		t.Fatal(err)
	}
	command := newRunnerCommand()
	command.SetArgs([]string{"render", "--source", gate, "--results-dir", dir})
	if err := command.Execute(); err == nil {
		t.Fatal("offline report claims success despite native suite failure")
	}
}

func completedOutcome(source e2eci.Frozen, image string) runOutcome {
	return runOutcome{Schema: 1, Source: source, Requested: source.Dispatch.Preset, Resolved: "standard", Workers: 20, Started: time.Unix(1, 0), Finished: time.Unix(2, 0), Image: image, ControllerCommit: source.HeadSHA, Infrastructure: statePassed, Tests: statePassed, Reports: statePassed, Cleanup: statePassed, Diagnostics: stateUnavailable, Artifact: stateNotRun, Publication: stateNotRun, Counts: specCounts{Executed: 1, Passed: 1}}
}

func TestUnfinishedOutcomeCannotBeSuccessful(t *testing.T) {
	_, source, image := preparedFixture(t)
	outcome := completedOutcome(source, image)
	outcome.Finished = time.Time{}
	if outcome.Overall() == statePassed {
		t.Fatal("unfinished lifecycle marked successful")
	}
	if err := outcome.validate(source); err == nil {
		t.Fatal("unfinished passed outcome accepted")
	}
}

func TestLifecycleSchemaAndPrimaryOutcome(t *testing.T) {
	_, source, image := preparedFixture(t)
	for _, mode := range []string{"schema", "source", "selection", "workers", "state", "negative-count", "count-sum", "zero", "controller", "image", "unfinished", "time-order"} {
		t.Run(mode, func(t *testing.T) {
			o := completedOutcome(source, image)
			switch mode {
			case "schema":
				o.Schema = 2
			case "source":
				o.Source.Dispatch.RunID++
			case "selection":
				o.Resolved = "gpu"
			case "workers":
				o.Workers = 1
			case "state":
				o.Artifact = "secret error message"
			case "negative-count":
				o.Counts.Failed = -1
			case "count-sum":
				o.Counts.Executed++
			case "zero":
				o.Counts = specCounts{}
			case "image":
				o.Image = "example.invalid/controller:mutable"
			case "controller":
				o.ControllerCommit = source.HeadSHA[:7]
			case "unfinished":
				o.Finished = time.Time{}
			case "time-order":
				o.Finished = o.Started.Add(-time.Second)
			}
			if err := o.validate(source); err == nil {
				t.Fatalf("invalid %s accepted", mode)
			}
		})
	}
	o := completedOutcome(source, image)
	if err := o.validate(source); err != nil {
		t.Fatal(err)
	}
	if o.Overall() != statePassed {
		t.Fatal("complete evidence not successful")
	}
	o.Artifact = stateFailed
	o.Publication = stateFailed
	if o.Overall() != statePassed || o.Tests != statePassed {
		t.Fatal("delivery failure erased tests")
	}
	o.Cleanup = stateFailed
	if o.Overall() != stateFailed || o.Tests != statePassed {
		t.Fatal("cleanup not independent")
	}
	o.Tests = stateCancelled
	if o.Overall() != stateCancelled || o.Cleanup != stateFailed {
		t.Fatal("primary cancellation erased by cleanup")
	}
}

func TestOfflineRenderRejectsMissingOrChangedEvidence(t *testing.T) {
	_, source, image := preparedFixture(t)
	for _, mode := range []string{"no-json", "no-junit", "count-mismatch", "unknown-schema-field", "duplicate-native-root"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			o := completedOutcome(source, image)
			writeNativeFixture(t, dir, types.Report{SuiteSucceeded: true, SpecReports: types.SpecReports{{LeafNodeType: types.NodeTypeIt, State: types.SpecStatePassed, LeafNodeText: "native passed"}}})
			if mode == "count-mismatch" {
				o.Counts = specCounts{Executed: 2, Passed: 2}
			}
			if err := persistOutcome(dir, o); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "no-json":
				if err := os.Remove(filepath.Join(dir, "results.json")); err != nil {
					t.Fatal(err)
				}
			case "no-junit":
				if err := os.Remove(filepath.Join(dir, "junit.xml")); err != nil {
					t.Fatal(err)
				}
			case "unknown-schema-field":
				path := filepath.Join(dir, "outcome.json")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append([]byte(`{"secret":"not allowed",`), data[1:]...), 0600); err != nil {
					t.Fatal(err)
				}
			case "duplicate-native-root":
				f, err := os.OpenFile(filepath.Join(dir, "junit.xml"), os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.WriteString("<testsuites/>"); err != nil {
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
			}
			gate := filepath.Join(t.TempDir(), "gate.json")
			data, _ := json.Marshal(source)
			if err := os.WriteFile(gate, data, 0600); err != nil {
				t.Fatal(err)
			}
			command := newRunnerCommand()
			command.SetArgs([]string{"render", "--source", gate, "--results-dir", dir})
			if err := command.Execute(); err == nil {
				t.Fatal("invalid native/outcome evidence rendered")
			}
			if _, err := os.Stat(filepath.Join(dir, "report.md")); !os.IsNotExist(err) {
				t.Fatal("invalid evidence left a report")
			}
		})
	}
}

func TestReportTruncatesRowsAndBoundsWorstCaseText(t *testing.T) {
	specs := make(types.SpecReports, 500)
	for i := range specs {
		specs[i] = types.SpecReport{LeafNodeType: types.NodeTypeIt, State: types.SpecStatePassed, LeafNodeText: strings.Repeat("<", 1000), LeafNodeLabels: []string{fmt.Sprintf("suite:feature-%d", i%40)}}
	}
	var output bytes.Buffer
	err := renderReport(&output, []types.Report{{SuiteSucceeded: true, SpecReports: specs}}, reportMetadata{})
	if output.Len() > 60000 {
		t.Fatal("bounded writer exceeded public output limit")
	}
	if err == nil && !strings.Contains(output.String(), "omitted") {
		t.Fatal("silent spec truncation")
	}
}

func TestReportDoesNotCallUnexecutedFeaturePassed(t *testing.T) {
	reports := []types.Report{{SuiteSucceeded: true, SpecReports: types.SpecReports{
		{LeafNodeType: types.NodeTypeIt, State: types.SpecStatePassed, LeafNodeText: "ran", LeafNodeLabels: []string{"suite:storage"}},
		{LeafNodeType: types.NodeTypeIt, State: types.SpecStateSkipped, LeafNodeText: "excluded", LeafNodeLabels: []string{"suite:gpu"}},
	}}}
	var output bytes.Buffer
	if err := renderReport(&output, reports, reportMetadata{}); err != nil {
		t.Fatal(err)
	}
	if strings.Count(output.String(), "<td>✅ pass</td>") != 1 {
		t.Fatalf("unexecuted feature displayed as pass: %s", output.String())
	}
}

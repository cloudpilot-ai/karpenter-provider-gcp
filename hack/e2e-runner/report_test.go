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
)

func TestWriteReport(t *testing.T) {
	dir := t.TempDir()
	data, err := json.Marshal([]types.Report{{
		SuitePath:      "/repo/test/suites/drift",
		SuiteSucceeded: true,
		RunTime:        4 * time.Second,
		SpecReports: types.SpecReports{{
			LeafNodeType: types.NodeTypeIt,
			LeafNodeText: "replaces a node",
			State:        types.SpecStatePassed,
			RunTime:      3 * time.Second,
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	jsonPath := filepath.Join(dir, "results.json")
	if err := os.WriteFile(jsonPath, data, 0644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "reports", "result.md")
	if err := writeReport(jsonPath, path, reportMetadata{Commit: "abc1234", ControllerCommit: "abc1234", Duration: "5s"}); err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), "<li>✅ pass · replaces a node (3s)</li>") ||
		!strings.Contains(string(output), "</details></td>\n<td>✅ pass</td><td>4s</td>") {
		t.Fatalf("unexpected report:\n%s", output)
	}
}

func TestRenderReport(t *testing.T) {
	reports := []types.Report{
		{
			SuitePath: "/repo/test/suites/provisioning",
			SpecReports: types.SpecReports{
				{LeafNodeType: types.NodeTypeIt, ContainerHierarchyTexts: []string{"Provisioning"}, LeafNodeText: "COS / amd64 | on-demand <check>", State: types.SpecStatePassed, RunTime: 88 * time.Second},
				{LeafNodeType: types.NodeTypeIt, ContainerHierarchyTexts: []string{"Provisioning"}, LeafNodeText: "COS / arm64", State: types.SpecStateFailed, RunTime: 12 * time.Second},
				{LeafNodeType: types.NodeTypeIt, LeafNodeText: "pending", State: types.SpecStatePending},
				{LeafNodeType: types.NodeTypeBeforeSuite, State: types.SpecStatePassed},
			},
		},
		{SuitePath: "/repo/test/suites/gpu", SpecialSuiteFailureReasons: []string{"could not compile"}},
	}
	var out bytes.Buffer
	if err := renderReport(&out, reports, reportMetadata{Commit: "abc1234-dirty", ControllerCommit: "abc1234", Duration: "4m30s", LogsUnavailable: true}); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		"<strong>Test commit:</strong> abc1234-dirty",
		"<strong>Controller commit:</strong> abc1234",
		"<strong>Total duration:</strong> 4m30s",
		"resource samples unavailable",
		"<strong>Controller logs:</strong> unavailable",
		"<th>Suite</th><th>Result</th><th>Duration</th>",
		"<summary>provisioning — 1 passed, 1 failed, 1 pending</summary>",
		"<li>✅ pass · Provisioning COS / amd64 | on-demand &lt;check&gt; (1m28s)</li>",
		"<li>❌ fail · Provisioning COS / arm64 (12s)</li>",
		"<summary>gpu — 0 passed, 1 failed</summary>",
		"<li>❌ fail · Suite failed to run: could not compile (—)</li>",
	} {
		if !strings.Contains(out.String(), text) {
			t.Errorf("report missing %q:\n%s", text, out.String())
		}
	}
	if got := strings.Count(out.String(), "<td><details>"); got != len(reports) {
		t.Errorf("got %d suite rows, want %d", got, len(reports))
	}
	if got := strings.Count(out.String(), "<table>"); got != 1 {
		t.Errorf("got %d tables, want only the suite table", got)
	}
	if strings.Contains(out.String(), "BeforeSuite") {
		t.Errorf("setup node in report:\n%s", out.String())
	}
}

func TestRenderUnifiedSuite(t *testing.T) {
	report := types.Report{
		SuitePath:      "/repo/test/suites",
		SuiteSucceeded: false,
		SpecReports: types.SpecReports{
			{LeafNodeType: types.NodeTypeIt, ContainerHierarchyLabels: [][]string{{"suite:provisioning"}}, LeafNodeText: "provisions a node", State: types.SpecStatePassed, RunTime: 4 * time.Second},
			{LeafNodeType: types.NodeTypeIt, ContainerHierarchyLabels: [][]string{{"suite:storage"}}, LeafNodeText: "attaches a disk", State: types.SpecStateFailed, RunTime: 2 * time.Second},
		},
	}
	var out bytes.Buffer
	if err := renderReport(&out, []types.Report{report}, reportMetadata{}); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		"<summary>provisioning — 1 passed, 0 failed</summary>",
		"<summary>storage — 0 passed, 1 failed</summary>",
		"<li>✅ pass · provisions a node (4s)</li>",
		"<li>❌ fail · attaches a disk (2s)</li>",
	} {
		if !strings.Contains(out.String(), text) {
			t.Errorf("report missing %q:\n%s", text, out.String())
		}
	}
	if got := strings.Count(out.String(), "<td><details>"); got != 2 {
		t.Errorf("got %d rows, want two feature rows", got)
	}
}

func TestRenderFeatureWallTime(t *testing.T) {
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	report := types.Report{SuiteSucceeded: true, SpecReports: types.SpecReports{
		{LeafNodeType: types.NodeTypeIt, ContainerHierarchyLabels: [][]string{{"suite:provisioning"}}, State: types.SpecStatePassed, StartTime: start, EndTime: start.Add(4 * time.Second), RunTime: 4 * time.Second},
		{LeafNodeType: types.NodeTypeIt, ContainerHierarchyLabels: [][]string{{"suite:provisioning"}}, State: types.SpecStatePassed, StartTime: start.Add(time.Second), EndTime: start.Add(5 * time.Second), RunTime: 4 * time.Second},
		{LeafNodeType: types.NodeTypeIt, ContainerHierarchyLabels: [][]string{{"suite:provisioning"}}, State: types.SpecStatePending},
	}}
	var out bytes.Buffer
	if err := renderReport(&out, []types.Report{report}, reportMetadata{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "<td>✅ pass</td><td>5s</td>") {
		t.Fatalf("feature duration should span overlapping specs:\n%s", out.String())
	}
}

func TestRenderSuiteSetupFailure(t *testing.T) {
	var out bytes.Buffer
	if err := renderReport(&out, []types.Report{{SuitePath: "/repo/test/suites", SpecialSuiteFailureReasons: []string{"BeforeSuite failed"}}}, reportMetadata{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Suite failed to run: BeforeSuite failed") {
		t.Fatalf("suite failure missing from report:\n%s", out.String())
	}
}

func TestControllerResourcesWithoutSamples(t *testing.T) {
	reports := []types.Report{{SpecReports: types.SpecReports{{CapturedGinkgoWriterOutput: "[resources] karpenter controller: requests cpu=100m memory=128.0MiB; no usage samples available\n"}}}}
	want := "requests cpu=100m memory=128.0MiB; usage samples unavailable"
	if got := controllerResources(reports); got != want {
		t.Fatalf("controllerResources() = %q, want %q", got, want)
	}
}

func TestControllerResources(t *testing.T) {
	reports := []types.Report{
		{SpecReports: types.SpecReports{{CapturedGinkgoWriterOutput: "[resources] karpenter controller: requests cpu=100m memory=128.0MiB; latest cpu=40m memory=90.0MiB; peak cpu=200m memory=256.0MiB; samples=10\n"}}},
		{SpecReports: types.SpecReports{{CapturedGinkgoWriterOutput: "[resources] karpenter controller: requests cpu=100m memory=128.0MiB; latest cpu=70m memory=80.0MiB; peak cpu=300m memory=192.0MiB; samples=7\n"}}},
	}
	want := "requests cpu=100m memory=128.0MiB; peak cpu=300m memory=256.0MiB"
	if got := controllerResources(reports); got != want {
		t.Fatalf("controllerResources() = %q, want %q", got, want)
	}
	if got := controllerResources(nil); got != "resource samples unavailable" {
		t.Fatalf("controllerResources(nil) = %q", got)
	}
}

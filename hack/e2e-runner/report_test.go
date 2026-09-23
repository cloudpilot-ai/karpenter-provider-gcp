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
	if err := writeReport(jsonPath, path); err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), "<td>replaces a node</td><td>✅ pass</td><td>3s</td>") ||
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
	if err := renderReport(&out, reports); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		"<th>Suite</th><th>Result</th><th>Duration</th>",
		"<summary>provisioning — 1 passed, 1 failed, 1 pending</summary>",
		"<td>Provisioning COS / amd64 | on-demand &lt;check&gt;</td><td>✅ pass</td><td>1m28s</td>",
		"<td>Provisioning COS / arm64</td><td>❌ fail</td><td>12s</td>",
		"<summary>gpu — 0 passed, 1 failed</summary>",
		"<td>Suite failed to run: could not compile</td><td>❌ fail</td><td>—</td>",
	} {
		if !strings.Contains(out.String(), text) {
			t.Errorf("report missing %q:\n%s", text, out.String())
		}
	}
	if got := strings.Count(out.String(), "<td><details>"); got != len(reports) {
		t.Errorf("got %d suite rows, want %d", got, len(reports))
	}
	if strings.Contains(out.String(), "BeforeSuite") {
		t.Errorf("setup node in report:\n%s", out.String())
	}
}

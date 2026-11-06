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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onsi/ginkgo/v2/types"
)

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

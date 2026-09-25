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
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2/types"
)

func TestSelectionFilters(t *testing.T) {
	for _, tc := range []struct {
		name                              string
		provisioning, gpu, drift, storage bool
	}{
		{"standard", true, false, true, true},
		{"gpu", false, true, false, false},
		{"all", true, true, true, true},
		{"provisioning", true, false, false, false},
		{"drift", false, false, true, false},
		{"drift,storage", false, false, true, true},
		{" drift , storage ", false, false, true, true},
		{"gpu,drift", false, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filter, err := selectFilter("../../test/suites", tc.name)
			if err != nil {
				t.Fatal(err)
			}
			if filter == "" {
				if !tc.provisioning || !tc.gpu || !tc.drift || !tc.storage {
					t.Fatal("empty filter must select all features")
				}
				return
			}
			match, err := types.ParseLabelFilter(filter)
			if err != nil {
				t.Fatal(err)
			}
			for _, check := range []struct {
				feature string
				want    bool
			}{
				{"provisioning", tc.provisioning},
				{"gpu", tc.gpu},
				{"drift", tc.drift},
				{"storage", tc.storage},
			} {
				if got := match([]string{"suite:" + check.feature}); got != check.want {
					t.Errorf("%s: got %t, want %t", check.feature, got, check.want)
				}
			}
		})
	}
	for _, name := range []string{"", "missing", "drift,missing", "drift,", ",drift", "standard,drift"} {
		if _, err := selectFilter("../../test/suites", name); err == nil {
			t.Errorf("invalid selection %q was accepted", name)
		}
	}
}

func TestGlobalParallelism(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("go", "run", "github.com/onsi/ginkgo/v2/ginkgo", "--procs=2", "--output-dir="+dir, "--json-report=parallel.json", "./testdata/parallel/")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("parallel fixture: %v\n%s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(dir, "parallel.json"))
	if err != nil {
		t.Fatal(err)
	}
	var reports []types.Report
	if err := json.Unmarshal(data, &reports); err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || !reports[0].SuiteSucceeded {
		t.Fatalf("expected one successful Ginkgo suite, got %+v", reports)
	}
	type event struct {
		at    time.Time
		delta int
	}
	var events []event
	features := map[string]int{}
	for _, spec := range reports[0].SpecReports {
		if spec.LeafNodeType != types.NodeTypeIt {
			continue
		}
		for _, label := range spec.Labels() {
			features[label]++
		}
		events = append(events, event{spec.StartTime, 1}, event{spec.EndTime, -1})
	}
	if features["suite:a"] != 2 || features["suite:b"] != 2 {
		t.Fatalf("unexpected spec selection: %v", features)
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].at.Equal(events[j].at) {
			return events[i].delta < events[j].delta
		}
		return events[i].at.Before(events[j].at)
	})
	active, peak := 0, 0
	for _, e := range events {
		active += e.delta
		if active > peak {
			peak = active
		}
	}
	if peak != 2 || active != 0 {
		t.Fatalf("peak active specs = %d, ending at %d; want peak 2 and end 0", peak, active)
	}

	standalone := exec.Command("go", "run", "github.com/onsi/ginkgo/v2/ginkgo", "--dry-run", "--output-dir="+dir, "--json-report=standalone.json", "./testdata/parallel/a/")
	if output, err := standalone.CombinedOutput(); err != nil {
		t.Fatalf("standalone fixture: %v\n%s", err, output)
	}
	data, err = os.ReadFile(filepath.Join(dir, "standalone.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &reports); err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].PreRunStats.SpecsThatWillRun != 2 {
		t.Fatalf("expected two standalone specs, got %+v", reports)
	}
}

func TestRootSuiteLabels(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("go", "run", "github.com/onsi/ginkgo/v2/ginkgo", "--dry-run", "--output-dir="+dir, "--json-report=root.json", "../../test/suites/")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("root suite dry run: %v\n%s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(dir, "root.json"))
	if err != nil {
		t.Fatal(err)
	}
	var reports []types.Report
	if err := json.Unmarshal(data, &reports); err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 {
		t.Fatalf("want one root report, got %d", len(reports))
	}
	seen := map[string]bool{}
	for _, spec := range reports[0].SpecReports {
		if spec.LeafNodeType != types.NodeTypeIt {
			continue
		}
		labeled := false
		for _, label := range spec.Labels() {
			if feature, ok := strings.CutPrefix(label, "suite:"); ok {
				seen[feature] = true
				labeled = true
			}
		}
		if !labeled {
			t.Errorf("spec %q has no suite label", spec.FullText())
		}
	}
	entries, err := os.ReadDir("../../test/suites")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() && !seen[entry.Name()] {
			t.Errorf("feature %s has no registered specs", entry.Name())
		}
	}
}

func TestRootSuiteImportsFeatureDirectories(t *testing.T) {
	root := filepath.Join("..", "..", "test", "suites")
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, "suite_test.go"), nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	imports := make(map[string]bool)
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(path, "/test/suites/") {
			imports[filepath.Base(path)] = true
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() && !imports[entry.Name()] {
			t.Errorf("feature directory %s not imported by root suite", entry.Name())
		}
	}
}

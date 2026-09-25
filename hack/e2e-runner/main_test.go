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
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2/types"
)

func TestCancelStopsSubprocesses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 & echo $!; wait")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- runIsolated(cmd) }()
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("reading child PID: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("canceled command succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("command did not stop after cancellation")
	}
	state, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "state=").Output()
	if err == nil && !strings.HasPrefix(strings.TrimSpace(string(state)), "Z") {
		t.Fatalf("child process %d still running: %s", pid, state)
	}
}

func TestLockID(t *testing.T) {
	if got, err := resolveLockID("ci-run"); err != nil || got != "ci-run" {
		t.Fatalf("override = %q, %v", got, err)
	}

	got, err := resolveLockID("")
	if err != nil {
		t.Fatal(err)
	}
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	suffix := "@" + hostname + ":pid-" + strconv.Itoa(os.Getpid())
	if !strings.HasSuffix(got, suffix) || len(got) <= len(suffix) {
		t.Fatalf("default lock ID %q does not match username%s", got, suffix)
	}
}

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

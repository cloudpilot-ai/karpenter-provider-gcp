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
	"errors"
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
	read := make(chan struct {
		line string
		err  error
	}, 1)
	go func() {
		line, err := bufio.NewReader(out).ReadString('\n')
		read <- struct {
			line string
			err  error
		}{line, err}
	}()
	var line string
	select {
	case result := <-read:
		if result.err != nil {
			t.Fatalf("reading child PID: %v", result.err)
		}
		line = result.line
	case <-time.After(5 * time.Second):
		t.Fatal("child process did not start")
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
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Fatalf("checking child process %d: %v", pid, err)
		}
	} else if !strings.HasPrefix(strings.TrimSpace(string(state)), "Z") {
		t.Fatalf("child process %d still running: %s", pid, state)
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

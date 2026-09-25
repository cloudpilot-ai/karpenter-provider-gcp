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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanupOnlyOwnedResources(t *testing.T) {
	dir := t.TempDir()
	kubectl := `#!/bin/sh
case "$1" in
  config) echo gke_project_us-central1_karpenter-e2e-cluster ;;
  get) echo "$3" ;;
  delete) echo "$*" >> "$COMMAND_LOG" ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(kubectl), 0755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "commands")
	cmd := exec.Command("bash", "../e2e-clean-env.sh")
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "COMMAND_LOG="+logPath, "E2E_PROJECT_ID=project", "E2E_LOCATION=us-central1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cleanup: %v\n%s", err, out)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range []string{"nodeclaims", "nodepools", "gcenodeclasses"} {
		if !strings.Contains(string(data), "delete "+resource+" --selector=karpenter-e2e/owned=true ") {
			t.Errorf("cleanup did not scope %s: %s", resource, data)
		}
	}
	if strings.Contains(string(data), "--all") {
		t.Fatalf("cleanup includes --all: %s", data)
	}
}

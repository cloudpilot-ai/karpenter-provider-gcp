/*
Copyright 2026 The CloudPilot AI Authors.

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
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestBootstrapCapabilities(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "runner")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, output)
	}
	output, err := exec.Command(binary, "capabilities").Output()
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Version  int
		Modes    map[string]string
		Commands []string
	}
	if err := json.Unmarshal(output, &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 || got.Modes["standard"] != "not_implemented" || len(got.Commands) != 4 {
		t.Fatalf("unexpected capabilities: %s", output)
	}
}

func TestBootstrapHandoffNeverClaimsExecution(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "runner")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, output)
	}
	for _, command := range []string{"prepare", "run", "report"} {
		t.Run(command, func(t *testing.T) {
			dir := t.TempDir()
			invocation := filepath.Join(dir, "invocation.json")
			output := filepath.Join(dir, "result.json")
			if err := os.WriteFile(invocation, []byte(`{"version":1,"run":"123"}`), 0600); err != nil {
				t.Fatal(err)
			}
			if data, err := exec.Command(binary, command, "--invocation", invocation, "--source", dir, "--bundle", dir, "--output", output).CombinedOutput(); err != nil {
				t.Fatalf("%v: %s", err, data)
			}
			data, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Version    int
				Status     string
				Executed   int
				Invocation map[string]any
			}
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if got.Version != 1 || got.Status != "not_implemented" || got.Executed != 0 || got.Invocation["run"] != "123" {
				t.Fatalf("unexpected handoff: %s", data)
			}
			if err := exec.Command(binary, command, "--invocation", "missing", "--output", output).Run(); err == nil {
				t.Fatal("missing invocation accepted")
			}
		})
	}
}

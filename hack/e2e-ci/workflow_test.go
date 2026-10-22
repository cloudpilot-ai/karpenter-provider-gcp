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

package e2eci

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type workflowStep struct {
	Name, Uses, Run, If string
	Env                 map[string]string
	With                map[string]any
}
type workflowJob struct {
	Needs       any
	Permissions map[string]string
	RunsOn      string `yaml:"runs-on"`
	Environment string
	If          string
	Steps       []workflowStep
	Concurrency struct {
		Group  string
		Cancel bool `yaml:"cancel-in-progress"`
	}
}

func readWorkflow(t *testing.T) (map[string]workflowJob, map[string]any) {
	t.Helper()
	data, err := os.ReadFile("../../.github/workflows/e2e.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On          map[string]any
		Permissions map[string]string
		Jobs        map[string]workflowJob
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	if len(workflow.Permissions) != 0 {
		t.Fatal("ambient workflow authority")
	}
	return workflow.Jobs, workflow.On
}
func TestWorkflowSeparatesAuthorityAndFreshWorkers(t *testing.T) {
	jobs, on := readWorkflow(t)
	if len(on) != 1 || on["workflow_dispatch"] == nil {
		t.Fatal("nonmanual trigger")
	}
	want := map[string]map[string]string{
		"gate":     {"contents": "read", "actions": "read", "pull-requests": "read"},
		"pending":  {"contents": "read", "checks": "write"},
		"build":    {"contents": "read"},
		"runtime":  {"contents": "read", "actions": "read", "id-token": "write"},
		"reporter": {"contents": "read", "actions": "read", "checks": "write", "pull-requests": "write"},
	}
	if len(jobs) != len(want) {
		t.Fatal("unexpected jobs")
	}
	for name, permissions := range want {
		job := jobs[name]
		if job.RunsOn != "ubuntu-24.04" {
			t.Fatalf("%s not fresh hosted worker", name)
		}
		if len(job.Permissions) != len(permissions) {
			t.Fatalf("%s permissions %#v", name, job.Permissions)
		}
		for key, value := range permissions {
			if job.Permissions[key] != value {
				t.Fatalf("%s %s grant", name, key)
			}
		}
		for _, step := range job.Steps {
			if strings.Contains(step.Run, "${{") {
				t.Fatalf("%s context interpolated into shell", name)
			}
			if step.Uses != "" && !regexp.MustCompile(`^[A-Za-z0-9_-]+/[A-Za-z0-9_-]+@[a-f0-9]{40}$`).MatchString(step.Uses) {
				t.Fatalf("unpinned/local action %s", step.Uses)
			}
			if strings.Contains(step.Uses, "actions/checkout@") {
				if step.With["ref"] != "${{ github.workflow_sha }}" || step.With["persist-credentials"] != false {
					t.Fatal("checkout not trusted/credential-free")
				}
			}
			if strings.Contains(step.Uses, "actions/setup-go@") && step.With["cache"] != false {
				t.Fatal("shared PR cache")
			}
			if name != "runtime" && (strings.Contains(step.Uses, "google-github-actions") || strings.Contains(step.Run, "gcloud ")) {
				t.Fatalf("cloud outside runtime: %s", name)
			}
			if name == "runtime" {
				for _, value := range step.Env {
					if strings.Contains(value, "secrets.") {
						t.Fatal("unrelated runtime secret")
					}
				}
			}
		}
	}
	if !strings.Contains(jobs["gate"].If, "github.run_attempt == 1") || !strings.Contains(jobs["gate"].If, "refs/heads/main") {
		t.Fatal("early rerun/ref gate absent")
	}
	needs := jobs["build"].Needs.([]any)
	if !slices.ContainsFunc(needs, func(v any) bool { return v == "pending" }) {
		t.Fatal("build can start on rejected fork check")
	}
	runtime := jobs["runtime"]
	if runtime.Environment != "e2e-runtime" || runtime.Concurrency.Cancel || runtime.Concurrency.Group != "${{ needs.gate.outputs.target }}" {
		t.Fatal("target lifecycle exclusion changed")
	}
	if jobs["reporter"].Concurrency.Cancel || !strings.Contains(jobs["reporter"].Concurrency.Group, "outputs.pr") || !strings.Contains(jobs["reporter"].Concurrency.Group, "outputs.preset") {
		t.Fatal("publication not serialized by PR/preset")
	}
}
func TestWorkflowVerifiesOriginalArtifactBeforeWIFAndNeverUploadsPrivateTrees(t *testing.T) {
	jobs, _ := readWorkflow(t)
	verified, authenticated, isolated, login := false, false, false, false
	for _, step := range jobs["runtime"].Steps {
		if strings.Contains(step.Run, "e2e-ci verify ") {
			if !strings.Contains(step.Run, "e2e-ci recheck ") || !strings.Contains(step.Run, "e2e-ci artifact ") || !strings.Contains(step.Run, "e2e-ci download ") {
				t.Fatal("incomplete preauth chain")
			}
			verified = true
		}
		if strings.Contains(step.Uses, "google-github-actions/auth@") {
			if !verified {
				t.Fatal("WIF before verification")
			}
			if step.With["audience"] == nil || step.With["token_format"] != nil {
				t.Fatal("missing explicit audience or fixed token")
			}
			authenticated = true
		}
		if strings.Contains(step.Run, "external_account") {
			if !authenticated || !strings.Contains(step.Run, "e2e-auth/external-account.json") {
				t.Fatal("credential isolation absent")
			}
			isolated = true
		}
		if strings.Contains(step.Run, "gcloud auth login --cred-file=") {
			if !isolated {
				t.Fatal("gcloud before credential isolation")
			}
			login = true
		}
		if strings.Contains(step.Run, "e2e-runner prepared ") {
			if !login || !strings.Contains(step.Run, "private-console.log") {
				t.Fatal("unauthenticated/unbounded public PR output")
			}
		}
		if strings.Contains(step.Uses, "actions/upload-artifact@") {
			if step.With["retention-days"] != 30 {
				t.Fatal("safe evidence not retained 30d")
			}
			path := step.With["path"].(string)
			if strings.ContainsAny(path, "*?") || strings.Contains(path, "private") || strings.Contains(path, "auth") {
				t.Fatal("private/glob upload")
			}
			for _, line := range strings.Split(strings.TrimSpace(path), "\n") {
				if !strings.HasPrefix(strings.TrimSpace(line), "${{ runner.temp }}/public-results/") {
					t.Fatal("unsafe upload path")
				}
			}
		}
	}
	if !verified || !authenticated || !login {
		t.Fatal("runtime chain missing")
	}
	for _, step := range jobs["reporter"].Steps {
		if strings.Contains(step.Run, "prepared ") || strings.Contains(step.Run, "ginkgo ") || strings.Contains(step.Run, "kubectl ") {
			t.Fatal("reporter executes PR/cloud code")
		}
	}
}

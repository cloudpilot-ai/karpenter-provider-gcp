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
	"strings"
	"testing"
	"time"
)

func TestRenderReportEscapesSpecTextAndPreservesFailure(t *testing.T) {
	result := RunResult{
		Version: schemaVersion,
		Plan:    RunPlan{Preset: "standard"},
		Status:  StatusFailed,
		Specs: []SpecResult{{
			Name:     "provisioning | rejects <unsafe>|value",
			Status:   StatusFailed,
			Duration: 2 * time.Second,
			Message:  "failure | details",
		}},
		Metrics: MetricCoverage{Available: false, Reason: "metrics API unavailable"},
	}

	got, err := renderReport(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"### E2E Test Results", "provisioning \\| rejects &lt;unsafe&gt;\\|value", "failure \\| details", "metrics API unavailable", "**Overall: ❌ failed**"} {
		if !strings.Contains(got, want) {
			t.Fatalf("report missing %q:\n%s", want, got)
		}
	}
}

func TestRenderReportRejectsInvalidResult(t *testing.T) {
	if _, err := renderReport(RunResult{Version: schemaVersion, Status: StatusPassed}); err == nil {
		t.Fatal("expected successful empty result to be rejected")
	}
}

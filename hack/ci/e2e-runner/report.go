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
	"fmt"
	"strings"
	"text/template"
	"time"
)

type RunStatus string

const (
	StatusPassed     RunStatus = "passed"
	StatusFailed     RunStatus = "failed"
	StatusIncomplete RunStatus = "incomplete"
)

type SpecResult struct {
	Name     string        `json:"name"`
	Status   RunStatus     `json:"status"`
	Duration time.Duration `json:"duration"`
	Message  string        `json:"message,omitempty"`
}

type MetricCoverage struct {
	Available bool             `json:"available"`
	Reason    string           `json:"reason,omitempty"`
	Samples   []ResourceSample `json:"samples,omitempty"`
}

type RunResult struct {
	Version int            `json:"version"`
	Plan    RunPlan        `json:"plan"`
	Status  RunStatus      `json:"status"`
	Specs   []SpecResult   `json:"specs"`
	Metrics MetricCoverage `json:"metrics"`
}

func renderReport(result RunResult) (string, error) {
	if result.Version != schemaVersion {
		return "", fmt.Errorf("unsupported result schema version %d", result.Version)
	}
	if result.Status == StatusPassed && len(result.Specs) == 0 {
		return "", fmt.Errorf("successful result has no specs")
	}
	if result.Status != StatusPassed && result.Status != StatusFailed && result.Status != StatusIncomplete {
		return "", fmt.Errorf("invalid result status %q", result.Status)
	}
	for i, spec := range result.Specs {
		if spec.Status != StatusPassed && spec.Status != StatusFailed && spec.Status != StatusIncomplete {
			return "", fmt.Errorf("invalid spec result")
		}
		if spec.Name == "" {
			result.Specs[i].Name = "suite setup failure (native report omitted a spec name)"
		}
	}
	var output strings.Builder
	tmpl := template.Must(template.New("report").Funcs(template.FuncMap{"escape": escapeMarkdown}).Parse(`### E2E Test Results

**Preset:** {{.Preset}}

| Spec | Result | Duration | Notes |
|---|---|---|---|
{{range .Specs}}| {{escape .Name}} | {{.Status}} | {{.Duration}} | {{escape .Message}} |
{{end}}
**Controller resources:** {{if .Metrics.Available}}samples available{{else}}{{escape .Metrics.Reason}}{{end}}

**Overall: {{.Overall}}**
`))
	data := struct {
		Preset  string
		Specs   []SpecResult
		Metrics MetricCoverage
		Overall string
	}{result.Plan.Preset, result.Specs, result.Metrics, overall(result.Status)}
	if err := tmpl.Execute(&output, data); err != nil {
		return "", err
	}
	return output.String(), nil
}

func overall(status RunStatus) string {
	switch status {
	case StatusPassed:
		return "✅ all pass"
	case StatusFailed:
		return "❌ failed"
	default:
		return "⚠️ incomplete"
	}
}

func escapeMarkdown(value string) string {
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	value = strings.ReplaceAll(value, ">", "&gt;")
	return strings.ReplaceAll(value, "|", "\\|")
}

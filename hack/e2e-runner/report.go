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
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2/types"
	"k8s.io/apimachinery/pkg/api/resource"
)

//go:embed report.gotmpl
var reportTemplate string

type specResult struct {
	Name, Result, Duration string
}

type suiteResult struct {
	Name, Result, Duration           string
	Passed, Failed, Skipped, Pending int
	Specs                            []specResult
}

type reportMetadata struct {
	Commit, ControllerCommit, Duration string
	LogsUnavailable                    bool
}

var resourceLine = regexp.MustCompile(`\[resources\] karpenter controller: requests cpu=(\d+)m memory=([^;]+); (?:latest cpu=\d+m memory=[^;]+; peak cpu=(\d+)m memory=([^;]+); samples=\d+|no usage samples available)`)

func controllerResources(reports []types.Report) string {
	var requestCPU, requestMemory, peakCPU, peakMemory string
	var maxCPU, maxMemory int64
	for _, report := range reports {
		for _, spec := range report.SpecReports {
			for _, match := range resourceLine.FindAllStringSubmatch(spec.CapturedGinkgoWriterOutput, -1) {
				if requestCPU == "" {
					requestCPU, requestMemory = match[1]+"m", match[2]
				}
				if match[3] == "" {
					continue
				}
				cpu, err := strconv.ParseInt(match[3], 10, 64)
				if err != nil {
					continue
				}
				memory, err := resource.ParseQuantity(strings.TrimSuffix(match[4], "B"))
				if err != nil {
					continue
				}
				if peakCPU == "" || cpu > maxCPU {
					maxCPU, peakCPU = cpu, match[3]+"m"
				}
				if peakMemory == "" || memory.Value() > maxMemory {
					maxMemory, peakMemory = memory.Value(), match[4]
				}
			}
		}
	}
	if requestCPU == "" {
		return "resource samples unavailable"
	}
	if peakCPU == "" {
		return fmt.Sprintf("requests cpu=%s memory=%s; usage samples unavailable", requestCPU, requestMemory)
	}
	return fmt.Sprintf("requests cpu=%s memory=%s; peak cpu=%s memory=%s", requestCPU, requestMemory, peakCPU, peakMemory)
}

func renderReport(w io.Writer, reports []types.Report, metadata reportMetadata) error {
	if len(reports) == 0 {
		return fmt.Errorf("Ginkgo produced no suite results")
	}
	var suites []suiteResult
	indices := map[string]int{}
	durations := map[string]time.Duration{}
	bounds := map[string]struct{ start, end time.Time }{}
	for _, report := range reports {
		fallback := filepath.Base(report.SuitePath)
		failedSpecs := 0
		for _, spec := range report.SpecReports {
			if spec.LeafNodeType != types.NodeTypeIt {
				continue
			}
			name := fallback
			for _, label := range spec.Labels() {
				if feature, ok := strings.CutPrefix(label, "suite:"); ok {
					name = feature
					break
				}
			}
			idx, exists := indices[name]
			if !exists {
				idx = len(suites)
				indices[name] = idx
				suites = append(suites, suiteResult{Name: name, Result: "✅ pass"})
			}
			suite := &suites[idx]
			row := specResult{Name: spec.FullText(), Duration: spec.RunTime.Round(time.Second).String()}
			switch {
			case spec.State == types.SpecStatePassed:
				row.Result = "✅ pass"
				suite.Passed++
			case spec.State.Is(types.SpecStateFailureStates):
				row.Result = "❌ fail"
				suite.Failed++
				failedSpecs++
				suite.Result = "❌ fail"
			case spec.State == types.SpecStateSkipped:
				row.Result = "⏭ skipped"
				suite.Skipped++
			case spec.State == types.SpecStatePending:
				row.Result = "⏭ pending"
				suite.Pending++
			default:
				continue
			}
			suite.Specs = append(suite.Specs, row)
			if name == fallback {
				suite.Duration = report.RunTime.Round(time.Second).String()
			} else if !spec.StartTime.IsZero() && !spec.EndTime.IsZero() {
				span := bounds[name]
				if span.start.IsZero() || spec.StartTime.Before(span.start) {
					span.start = spec.StartTime
				}
				if spec.EndTime.After(span.end) {
					span.end = spec.EndTime
				}
				bounds[name] = span
				suite.Duration = span.end.Sub(span.start).Round(time.Second).String()
			} else if bounds[name].start.IsZero() {
				durations[name] += spec.RunTime
				suite.Duration = durations[name].Round(time.Second).String()
			}
		}
		if !report.SuiteSucceeded && failedSpecs == 0 {
			idx, exists := indices[fallback]
			if !exists {
				idx = len(suites)
				indices[fallback] = idx
				suites = append(suites, suiteResult{Name: fallback, Duration: report.RunTime.Round(time.Second).String()})
			}
			reason := strings.Join(report.SpecialSuiteFailureReasons, "; ")
			if reason == "" {
				reason = "unknown failure"
			}
			suite := &suites[idx]
			suite.Result = "❌ fail"
			suite.Specs = append(suite.Specs, specResult{Name: "Suite failed to run: " + reason, Result: "❌ fail", Duration: "—"})
			suite.Failed++
		}
	}
	sort.Slice(suites, func(i, j int) bool { return suites[i].Name < suites[j].Name })
	tmpl, err := template.New("report").Parse(reportTemplate)
	if err != nil {
		return err
	}
	return tmpl.Execute(w, struct {
		Suites           []suiteResult
		Commit           string
		ControllerCommit string
		Duration         string
		Resources        string
		LogsUnavailable  bool
	}{suites, metadata.Commit, metadata.ControllerCommit, metadata.Duration, controllerResources(reports), metadata.LogsUnavailable})
}

func writeReport(jsonPath, path string, metadata reportMetadata) error {
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return fmt.Errorf("reading Ginkgo JSON report: %w", err)
	}
	var reports []types.Report
	if err := json.Unmarshal(data, &reports); err != nil {
		return fmt.Errorf("decoding Ginkgo JSON report: %w", err)
	}
	var output bytes.Buffer
	if err := renderReport(&output, reports, metadata); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, output.Bytes(), 0644)
}

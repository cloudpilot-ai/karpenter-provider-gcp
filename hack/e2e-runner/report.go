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

var resourceLine = regexp.MustCompile(`\[resources\] karpenter controller: requests cpu=(\d+)m memory=([^;]+); latest cpu=\d+m memory=[^;]+; peak cpu=(\d+)m memory=([^;]+); samples=\d+`)

func controllerResources(reports []types.Report) string {
	var requestCPU, requestMemory, peakCPU, peakMemory string
	var maxCPU, maxMemory int64
	for _, report := range reports {
		for _, spec := range report.SpecReports {
			for _, match := range resourceLine.FindAllStringSubmatch(spec.CapturedGinkgoWriterOutput, -1) {
				cpu, err := strconv.ParseInt(match[3], 10, 64)
				if err != nil {
					continue
				}
				memory, err := resource.ParseQuantity(strings.TrimSuffix(match[4], "B"))
				if err != nil {
					continue
				}
				if requestCPU == "" {
					requestCPU, requestMemory = match[1]+"m", match[2]
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
	return fmt.Sprintf("requests cpu=%s memory=%s; peak cpu=%s memory=%s", requestCPU, requestMemory, peakCPU, peakMemory)
}

func renderReport(w io.Writer, reports []types.Report, metadata reportMetadata) error {
	if len(reports) == 0 {
		return fmt.Errorf("Ginkgo produced no suite results")
	}
	var suites []suiteResult
	for _, report := range reports {
		suite := suiteResult{Name: filepath.Base(report.SuitePath), Duration: report.RunTime.Round(time.Second).String(), Result: "✅ pass"}
		if !report.SuiteSucceeded {
			suite.Result = "❌ fail"
		}
		for _, spec := range report.SpecReports {
			if spec.LeafNodeType != types.NodeTypeIt {
				continue
			}
			row := specResult{Name: spec.FullText(), Duration: spec.RunTime.Round(time.Second).String()}
			switch {
			case spec.State == types.SpecStatePassed:
				row.Result = "✅ pass"
				suite.Passed++
			case spec.State.Is(types.SpecStateFailureStates):
				row.Result = "❌ fail"
				suite.Failed++
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
		}
		if !report.SuiteSucceeded && suite.Failed == 0 {
			reason := strings.Join(report.SpecialSuiteFailureReasons, "; ")
			if reason == "" {
				reason = "unknown failure"
			}
			suite.Specs = append(suite.Specs, specResult{Name: "Suite failed to run: " + reason, Result: "❌ fail", Duration: "—"})
			suite.Failed++
		}
		suites = append(suites, suite)
	}
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

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
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/onsi/ginkgo/v2/types"
)

//go:embed report.gotmpl
var reportTemplate string

type specResult struct {
	Suite, Name, Result, Duration string
}

type suiteResult struct {
	Name                             string
	Passed, Failed, Skipped, Pending int
	Specs                            []specResult
}

func renderReport(w io.Writer, reports []types.Report) error {
	if len(reports) == 0 {
		return fmt.Errorf("Ginkgo produced no suite results")
	}
	var suites []suiteResult
	for _, report := range reports {
		suite := suiteResult{Name: filepath.Base(report.SuitePath)}
		for _, spec := range report.SpecReports {
			if spec.LeafNodeType != types.NodeTypeIt {
				continue
			}
			row := specResult{Suite: suite.Name, Name: spec.FullText(), Duration: spec.RunTime.Round(time.Second).String()}
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
			suite.Specs = append(suite.Specs, specResult{Suite: suite.Name, Name: "Suite failed to run: " + reason, Result: "❌ fail", Duration: "—"})
			suite.Failed++
		}
		suites = append(suites, suite)
	}
	escape := strings.NewReplacer("|", "\\|", "\n", " ", "\r", " ").Replace
	tmpl, err := template.New("report").Funcs(template.FuncMap{"escape": escape}).Parse(reportTemplate)
	if err != nil {
		return err
	}
	return tmpl.Execute(w, struct{ Suites []suiteResult }{suites})
}

func writeReport(jsonPath, path string) error {
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return fmt.Errorf("reading Ginkgo JSON report: %w", err)
	}
	var reports []types.Report
	if err := json.Unmarshal(data, &reports); err != nil {
		return fmt.Errorf("decoding Ginkgo JSON report: %w", err)
	}
	var output bytes.Buffer
	if err := renderReport(&output, reports); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, output.Bytes(), 0644)
}

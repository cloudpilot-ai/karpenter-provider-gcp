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
	"unicode"

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
	ControllerDigest                   string
	LogsUnavailable                    bool
	Outcome                            *runOutcome
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
	if len(reports) == 0 && metadata.Outcome == nil {
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
	omitted, remaining := 0, 64
	for i := range suites {
		if suites[i].Passed == 0 && suites[i].Failed == 0 {
			suites[i].Result = "⏭ not executed"
		}
		keep := min(len(suites[i].Specs), remaining)
		if i >= 32 {
			keep = 0
		}
		omitted += len(suites[i].Specs) - keep
		suites[i].Specs = suites[i].Specs[:keep]
		remaining -= keep
	}
	if len(suites) > 32 {
		suites = suites[:32]
	}
	tmpl, err := template.New("report").Funcs(template.FuncMap{"public": publicText}).Parse(reportTemplate)
	if err != nil {
		return err
	}
	return tmpl.Execute(&limitedWriter{Writer: w, Remaining: 60000}, struct {
		Suites           []suiteResult
		Commit           string
		ControllerCommit string
		ControllerDigest string
		Duration         string
		Resources        string
		LogsUnavailable  bool
		Outcome          *runOutcome
		Omitted          int
	}{suites, metadata.Commit, metadata.ControllerCommit, metadata.ControllerDigest, metadata.Duration, controllerResources(reports), metadata.LogsUnavailable, metadata.Outcome, omitted})
}

func writeReport(jsonPath, path string, metadata reportMetadata) error {
	reports, _, err := readJSONReports(jsonPath)
	if err != nil {
		return fmt.Errorf("reading Ginkgo JSON report: %w", err)
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

// renderPreparedReport reuses the sole trusted formatter; native evidence stays
// private. Callers must not upload its inputs or treat rendered spec text as secret-free.
func renderPreparedReport(dir string, outcome runOutcome) error {
	var reports []types.Report
	if outcome.Reports == statePassed {
		var counts specCounts
		var err error
		reports, counts, err = readNativeReports(dir)
		if err != nil {
			return err
		}
		if outcome.Tests == statePassed {
			for _, report := range reports {
				if !report.SuiteSucceeded {
					return fmt.Errorf("passed outcome contradicts native suite failure")
				}
			}
		}
		if counts != outcome.Counts {
			return fmt.Errorf("native counts differ from outcome")
		}
	}
	duration := "unavailable"
	if !outcome.Finished.IsZero() {
		duration = outcome.Finished.Sub(outcome.Started).Round(time.Second).String()
	}
	digest := ""
	if preparedImagePattern.MatchString(outcome.Image) {
		digest = strings.SplitN(outcome.Image, "@", 2)[1]
	}
	var output bytes.Buffer
	if err := renderReport(&output, reports, reportMetadata{Commit: outcome.Source.HeadSHA, ControllerCommit: outcome.ControllerCommit, ControllerDigest: digest, Duration: duration, LogsUnavailable: outcome.Diagnostics != statePassed, Outcome: &outcome}); err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, "report.md"), output.Bytes())
}

// publicText emits only text/entities, never PR HTML or Markdown. Controls and
// bidi formatting are discarded; truncation is explicit and output is bounded.
func publicText(value string) template.HTML {
	var out strings.Builder
	count := 0
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			if unicode.IsSpace(r) && count < 128 {
				count++
				out.WriteByte(' ')
			}
			continue
		}
		if count >= 128 {
			out.WriteString("… [truncated]")
			break
		}
		count++
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ':
			out.WriteRune(r)
		default:
			fmt.Fprintf(&out, "&#%d;", r)
		}
	}
	return template.HTML(out.String()) // All punctuation is numeric-entity encoded above.
}

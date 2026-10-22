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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2/reporters"
	"github.com/onsi/ginkgo/v2/types"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/resource"

	e2eci "github.com/cloudpilot-ai/karpenter-provider-gcp/hack/e2e-ci"
)

// Free text cannot be made public-safe by Markdown escaping or credential regexes.
// Project native reports onto IDs, known feature labels, enum states and numbers.
var publicFeatures = strings.Fields("channel-image-selection confidential consolidation customtype drift expiration gc gpu hugepages interruption kubelet_config local-ssd local-ssd-extended nested-virtualization networking provisioning repair storage")

func publicFeature(labels []string) []string {
	for _, label := range labels {
		for _, feature := range publicFeatures {
			if label == "suite:"+feature {
				return []string{label}
			}
		}
	}
	return nil
}
func publicDuration(d time.Duration) time.Duration {
	if d < 0 || d > 24*time.Hour {
		return 0
	}
	return d.Round(time.Millisecond)
}
func publicResources(text string) string {
	for _, m := range resourceLine.FindAllStringSubmatch(text, -1) {
		request, err := resource.ParseQuantity(strings.TrimSuffix(m[2], "B"))
		if err != nil || request.Value() < 0 || request.Value() > 1<<40 {
			continue
		}
		if m[3] == "" {
			return fmt.Sprintf("[resources] karpenter controller: requests cpu=%sm memory=%dB; no usage samples available", m[1], request.Value())
		}
		peak, err := resource.ParseQuantity(strings.TrimSuffix(m[4], "B"))
		if err != nil || peak.Value() < 0 || peak.Value() > 1<<40 {
			continue
		}
		return fmt.Sprintf("[resources] karpenter controller: requests cpu=%sm memory=%dB; latest cpu=0m memory=0B; peak cpu=%sm memory=%dB; samples=1", m[1], request.Value(), m[3], peak.Value())
	}
	return ""
}
func projectReports(reports []types.Report) []types.Report {
	safe := make([]types.Report, 0, len(reports))
	index := 0
	for i, report := range reports {
		r := types.Report{SuitePath: "e2e", SuiteDescription: fmt.Sprintf("suite-%02d", i+1), SuiteSucceeded: report.SuiteSucceeded, RunTime: publicDuration(report.RunTime)}
		for _, spec := range report.SpecReports {
			if spec.LeafNodeType != types.NodeTypeIt {
				continue
			}
			index++
			r.SpecReports = append(r.SpecReports, types.SpecReport{LeafNodeType: types.NodeTypeIt, LeafNodeText: fmt.Sprintf("spec-%05d", index), LeafNodeLabels: publicFeature(spec.Labels()), State: spec.State, RunTime: publicDuration(spec.RunTime), CapturedGinkgoWriterOutput: publicResources(spec.CapturedGinkgoWriterOutput)})
		}
		safe = append(safe, r)
	}
	return safe
}
func writePublicNative(dir string, reports []types.Report) error {
	data, err := json.Marshal(reports)
	if err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(dir, "results.json"), data); err != nil {
		return err
	}
	var paths []string
	for i, r := range reports {
		path := filepath.Join(dir, fmt.Sprintf(".junit-%d.xml", i))
		if err := reporters.GenerateJUnitReport(r, path); err != nil {
			return err
		}
		paths = append(paths, path)
	}
	_, err = reporters.MergeAndCleanupJUnitReports(paths, filepath.Join(dir, "junit.xml"))
	return err
}
func exportPublic(dir, out string, source e2eci.Frozen) (retErr error) {
	data, err := readBoundedRegular(filepath.Join(dir, "outcome.json"), e2eci.MaxMetadataBytes)
	if err != nil {
		return err
	}
	var outcome runOutcome
	if err := e2eci.DecodeStrict(bytes.NewReader(data), e2eci.MaxMetadataBytes, &outcome); err != nil {
		return err
	}
	if err := outcome.validate(source); err != nil {
		return err
	}
	// Bound individual counters before any sums can overflow on hostile evidence.
	for _, count := range []int{outcome.Counts.Executed, outcome.Counts.Passed, outcome.Counts.Failed, outcome.Counts.Skipped} {
		if count > maxNativeSpecs {
			return errors.New("public spec count exceeds limit")
		}
	}
	var reports []types.Report
	if outcome.Reports == statePassed {
		var counts specCounts
		reports, counts, err = readNativeReports(dir)
		if err != nil {
			return err
		}
		if counts != outcome.Counts {
			return errors.New("native counts differ from outcome")
		}
		for _, r := range reports {
			if outcome.Tests == statePassed && !r.SuiteSucceeded {
				return errors.New("native failure contradicts outcome")
			}
		}
	}
	if err := os.Mkdir(out, 0700); err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			_ = os.RemoveAll(out)
		}
	}()
	if outcome.Image != "" {
		if !preparedImagePattern.MatchString(outcome.Image) {
			return errors.New("invalid controller image")
		}
		outcome.Image = "public.invalid/controller@" + strings.SplitN(outcome.Image, "@", 2)[1]
	}
	// Source tuple and controller SHA are independently verified against the gate.
	if outcome.ControllerCommit != "" && outcome.ControllerCommit != source.HeadSHA {
		return errors.New("invalid controller commit")
	}
	outcome.Diagnostics = stateUnavailable
	if err := persistOutcome(out, outcome); err != nil {
		return err
	}
	if len(reports) > 0 {
		if err := writePublicNative(out, projectReports(reports)); err != nil {
			return err
		}
	}
	if err := renderPreparedReport(out, outcome); err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(out, "suite.log"), []byte(fmt.Sprintf("Raw suite output and identifiers omitted (not safely exportable). executed=%d passed=%d failed=%d skipped=%d\n", outcome.Counts.Executed, outcome.Counts.Passed, outcome.Counts.Failed, outcome.Counts.Skipped))); err != nil {
		return err
	}
	findings := publicLogFindings(dir)
	data, err = json.Marshal(findings)
	if err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(out, "controller.log"), data); err != nil {
		return err
	}
	report, err := readBoundedRegular(filepath.Join(out, "report.md"), 60000)
	if err != nil {
		return err
	}
	report = append(report, []byte(fmt.Sprintf("\n<p>Controller numeric log findings: errors=%d warnings=%d; parsed=%d omitted=%d; available=%t. Raw content unavailable.</p>\n", findings.Errors, findings.Warnings, findings.Parsed, findings.Omitted, findings.Available))...)
	if len(report) > 60000 {
		return errors.New("public report too large")
	}
	return atomicWrite(filepath.Join(out, "report.md"), report)
}
func newExportCommand() *cobra.Command {
	var sourcePath, dir, out string
	c := &cobra.Command{Use: "export", Short: "Allowlisted public projection; never upload raw runtime evidence", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		data, err := readBoundedRegular(sourcePath, e2eci.MaxMetadataBytes)
		if err != nil {
			return err
		}
		var source e2eci.Frozen
		if err := e2eci.DecodeStrict(bytes.NewReader(data), e2eci.MaxMetadataBytes, &source); err != nil {
			return err
		}
		if err := exportPublic(dir, out, source); err != nil {
			return err
		}
		data, err = readBoundedRegular(filepath.Join(out, "outcome.json"), e2eci.MaxMetadataBytes)
		if err != nil {
			return err
		}
		var outcome runOutcome
		if err := e2eci.DecodeStrict(bytes.NewReader(data), e2eci.MaxMetadataBytes, &outcome); err != nil {
			return err
		}
		verdict := "failure"
		if outcome.Overall() == statePassed {
			verdict = "success"
		} else if outcome.Overall() == stateCancelled {
			verdict = "cancelled"
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), verdict)
		return err
	}}
	c.Flags().StringVar(&sourcePath, "source", "", "Independent trusted gate JSON")
	c.Flags().StringVar(&dir, "results-dir", "", "Private evidence directory")
	c.Flags().StringVar(&out, "output", "", "New public-only directory")
	for _, key := range []string{"source", "results-dir", "output"} {
		_ = c.MarkFlagRequired(key)
	}
	return c
}
func newValidateTargetCommand() *cobra.Command {
	return &cobra.Command{Use: "validate-target", Short: "Cloud-free fixed prepared target validation", Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error { _, err := loadLifecycleTarget(true); return err }}
}

// Numeric schema supports safe re-projection on the separate reporter worker.
type logFindings struct {
	Schema    int  `json:"schema"`
	Available bool `json:"available"`
	Errors    int  `json:"errors"`
	Warnings  int  `json:"warnings"`
	Parsed    int  `json:"parsed"`
	Omitted   int  `json:"omitted"`
}

func publicLogFindings(dir string) logFindings {
	findings := logFindings{Schema: 1}
	data, err := readBoundedRegular(filepath.Join(dir, "controller.log"), 1<<20)
	if err != nil {
		return findings
	}
	var projected logFindings
	if e2eci.DecodeStrict(bytes.NewReader(data), 1<<20, &projected) == nil && projected.Schema == 1 && projected.Errors >= 0 && projected.Errors <= 10000 && projected.Warnings >= 0 && projected.Warnings <= 10000 && projected.Parsed >= 0 && projected.Parsed <= 10000 && projected.Omitted >= 0 && projected.Omitted <= 10000 && projected.Parsed >= projected.Errors+projected.Warnings && projected.Parsed+projected.Omitted <= 10000 {
		return projected
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		if findings.Parsed+findings.Omitted >= 10000 {
			return logFindings{Schema: 1}
		}
		var line struct {
			Severity string `json:"severity"`
			Level    string `json:"level"`
		}
		data := scanner.Text()
		// kubectl --all-pods adds a source prefix; --timestamps adds RFC3339.
		if strings.HasPrefix(data, "[pod/") {
			if _, rest, ok := strings.Cut(data, "] "); ok {
				data = rest
			}
		}
		if timestamp, rest, ok := strings.Cut(data, " "); ok {
			if _, err := time.Parse(time.RFC3339Nano, timestamp); err == nil {
				data = rest
			}
		}
		if json.Unmarshal([]byte(data), &line) != nil {
			findings.Omitted++
			continue
		}
		level := strings.ToLower(line.Severity)
		if level == "" {
			level = strings.ToLower(line.Level)
		}
		switch level {
		case "error", "fatal", "critical", "alert", "emergency":
			findings.Errors++
		case "warn", "warning":
			findings.Warnings++
		case "debug", "info", "notice", "trace":
		default:
			findings.Omitted++
			continue
		}
		findings.Parsed++
	}
	if scanner.Err() != nil {
		return logFindings{Schema: 1}
	}
	findings.Available = findings.Parsed > 0
	return findings
}

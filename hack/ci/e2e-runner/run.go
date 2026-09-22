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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/onsi/ginkgo/v2/types"
)

func executePlan(ctx context.Context, plan RunPlan, artifacts string) (RunResult, error) {
	if plan.Version != schemaVersion || len(plan.Waves) == 0 {
		return RunResult{}, fmt.Errorf("invalid execution plan")
	}
	if err := os.MkdirAll(artifacts, 0o750); err != nil {
		return RunResult{}, err
	}
	ownership, err := acquireOwnership(ctx, plan.Target, artifacts)
	if err != nil {
		return RunResult{}, err
	}
	defer ownership.release(context.Background())
	result := RunResult{Version: schemaVersion, Plan: plan, Status: StatusPassed}
	for _, wave := range plan.Waves {
		coverage, samples := collectControllerMetrics(ctx)
		if !coverage.Available {
			result.Metrics = coverage
		} else if !result.Metrics.Available {
			result.Metrics = coverage
			result.Metrics.Samples = append(result.Metrics.Samples, samples...)
		} else {
			result.Metrics.Samples = append(result.Metrics.Samples, samples...)
		}
		type suiteOutcome struct {
			specs []SpecResult
			err   error
		}
		outcomes := make(chan suiteOutcome, len(wave))
		var group sync.WaitGroup
		for _, suite := range wave {
			group.Add(1)
			go func(suite PlannedSuite) {
				defer group.Done()
				specs, err := executeSuite(ctx, plan, suite, artifacts)
				outcomes <- suiteOutcome{specs, err}
			}(suite)
		}
		group.Wait()
		close(outcomes)
		for outcome := range outcomes {
			result.Specs = append(result.Specs, outcome.specs...)
			if outcome.err != nil {
				result.Status = StatusFailed
			}
		}
		coverage, samples = collectControllerMetrics(ctx)
		if coverage.Available {
			result.Metrics.Available = true
			result.Metrics.Samples = append(result.Metrics.Samples, samples...)
		} else if !result.Metrics.Available {
			result.Metrics = coverage
		}
	}
	if len(result.Specs) == 0 {
		result.Status = StatusIncomplete
	}
	if result.Status != StatusPassed {
		return result, fmt.Errorf("e2e run %s", result.Status)
	}
	return result, nil
}

func executeSuite(ctx context.Context, plan RunPlan, suite PlannedSuite, artifacts string) ([]SpecResult, error) {
	suiteDir := filepath.Join("test", "suites", suite.Name)
	jsonPath := filepath.Join(artifacts, suite.Name+".json")
	logPath := filepath.Join(artifacts, suite.Name+".log")
	args := []string{"run", "github.com/onsi/ginkgo/v2/ginkgo", "--procs=" + fmt.Sprint(suite.Workers), "--timeout=30m", "--json-report=" + jsonPath, "--junit-report=" + filepath.Join(artifacts, suite.Name+".xml"), "-v"}
	if plan.Focus != "" {
		args = append(args, "--focus="+plan.Focus)
	}
	args = append(args, "./"+suiteDir)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Env = os.Environ()
	log, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	defer log.Close()
	cmd.Stdout, cmd.Stderr = log, log
	runErr := cmd.Run()
	specs, reportErr := readGinkgoReport(jsonPath)
	if reportErr != nil {
		return nil, fmt.Errorf("%w; native report: %v", runErr, reportErr)
	}
	if runErr != nil {
		return specs, runErr
	}
	return specs, nil
}

func readGinkgoReport(path string) ([]SpecResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var reports []types.Report
	if err := json.Unmarshal(data, &reports); err != nil {
		return nil, err
	}
	var results []SpecResult
	for _, report := range reports {
		for _, spec := range report.SpecReports {
			if spec.State.Is(types.SpecStateSkipped | types.SpecStatePending) {
				continue
			}
			status := StatusPassed
			if !spec.State.Is(types.SpecStatePassed) {
				status = StatusFailed
			}
			name := strings.TrimSpace(strings.Join(append(spec.ContainerHierarchyTexts, spec.LeafNodeText), " "))
			if name == "" {
				name = report.SuiteDescription + " suite setup"
			}
			results = append(results, SpecResult{Name: name, Status: status, Duration: spec.RunTime, Message: spec.Failure.Message})
		}
		if !report.SuiteSucceeded && len(report.SpecReports) == 0 {
			results = append(results, SpecResult{Name: report.SuiteDescription + " suite setup", Status: StatusFailed, Message: strings.Join(report.SpecialSuiteFailureReasons, "; ")})
		}
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("native report has no executed specs")
	}
	return results, nil
}

func writeResult(path string, result RunResult) error {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

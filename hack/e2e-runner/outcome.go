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
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2/reporters"
	"github.com/onsi/ginkgo/v2/types"

	e2eci "github.com/cloudpilot-ai/karpenter-provider-gcp/hack/e2e-ci"
)

const maxNativeBytes = 16 << 20
const maxNativeSpecs = 20000

type outcomeState string

const (
	stateNotRun      outcomeState = "not-run"
	stateRunning     outcomeState = "running"
	statePassed      outcomeState = "passed"
	stateFailed      outcomeState = "failed"
	stateCancelled   outcomeState = "cancelled"
	stateIncomplete  outcomeState = "incomplete"
	stateUnavailable outcomeState = "unavailable"
)

type specCounts struct {
	Executed int `json:"executed"`
	Passed   int `json:"passed"`
	Failed   int `json:"failed"`
	Skipped  int `json:"skipped"`
}

// runOutcome is private runtime evidence, not an upload allowlist. The lifecycle
// owner updates cleanup/diagnostics/export/publication independently of tests.
type runOutcome struct {
	MaintenanceRequired bool         `json:"maintenance_required"`
	Schema              int          `json:"schema"`
	Source              e2eci.Frozen `json:"source"`
	Requested           string       `json:"requested_selection"`
	Resolved            string       `json:"resolved_selection"`
	Workers             int          `json:"workers"`
	Started             time.Time    `json:"started"`
	Finished            time.Time    `json:"finished"`
	Image               string       `json:"image"`
	ControllerCommit    string       `json:"controller_commit"`
	Infrastructure      outcomeState `json:"infrastructure"`
	Tests               outcomeState `json:"tests"`
	Reports             outcomeState `json:"reports"`
	Cleanup             outcomeState `json:"cleanup"`
	Diagnostics         outcomeState `json:"diagnostics"`
	Artifact            outcomeState `json:"artifact"`
	Publication         outcomeState `json:"publication"`
	Counts              specCounts   `json:"counts"`
}

// Overall deliberately cannot claim success before the lifecycle owner records
// successful cleanup. Export/publication remain separate delivery outcomes.
func (o runOutcome) Overall() outcomeState {
	for _, state := range []outcomeState{o.Infrastructure, o.Tests, o.Reports, o.Cleanup} {
		if state == stateCancelled {
			return stateCancelled
		}
	}
	for _, state := range []outcomeState{o.Infrastructure, o.Tests, o.Reports, o.Cleanup} {
		if state == stateFailed {
			return stateFailed
		}
	}
	if !o.MaintenanceRequired && o.Infrastructure == statePassed && o.Tests == statePassed && o.Reports == statePassed && o.Cleanup == statePassed && o.Counts.Executed > 0 && !o.Finished.IsZero() {
		return statePassed
	}
	return stateIncomplete
}

func readBoundedRegular(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > limit {
		return nil, errors.New("missing, unsafe or oversized report")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(data)) > limit {
		return nil, errors.New("oversized report")
	}
	return data, err
}

func readJSONReports(path string) ([]types.Report, specCounts, error) {
	data, err := readBoundedRegular(path, maxNativeBytes)
	if err != nil {
		return nil, specCounts{}, err
	}
	var reports []types.Report
	if err := json.Unmarshal(data, &reports); err != nil {
		return nil, specCounts{}, err
	}
	var counts specCounts
	total := 0
	if len(reports) == 0 || len(reports) > 64 {
		return nil, counts, errors.New("invalid suite count")
	}
	for _, report := range reports {
		total += len(report.SpecReports)
		if total > maxNativeSpecs {
			return nil, counts, errors.New("too many specs")
		}
		for _, spec := range report.SpecReports {
			if spec.LeafNodeType != types.NodeTypeIt {
				continue
			}
			switch {
			case spec.State == types.SpecStatePassed:
				counts.Passed++
				counts.Executed++
			case spec.State.Is(types.SpecStateFailureStates):
				counts.Failed++
				counts.Executed++
			case spec.State.Is(types.SpecStateSkipped | types.SpecStatePending):
				counts.Skipped++
			default:
				return nil, counts, errors.New("incomplete spec state")
			}
		}
	}
	if counts.Executed == 0 {
		return reports, counts, errors.New("no executed tests")
	}
	return reports, counts, nil
}

func readNativeReports(dir string) ([]types.Report, specCounts, error) {
	reports, counts, err := readJSONReports(filepath.Join(dir, "results.json"))
	if err != nil {
		return reports, counts, err
	}
	data, err := readBoundedRegular(filepath.Join(dir, "junit.xml"), maxNativeBytes)
	if err != nil {
		return reports, counts, err
	}
	var junit reporters.JUnitTestSuites
	decoder := xml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&junit); err != nil {
		return reports, counts, err
	}
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return reports, counts, err
		}
		if whitespace, ok := token.(xml.CharData); !ok || strings.TrimSpace(string(whitespace)) != "" {
			return reports, counts, errors.New("trailing JUnit data")
		}
	}
	expected := map[string]int{}
	for _, report := range reports {
		for _, spec := range report.SpecReports {
			if spec.LeafNodeType == types.NodeTypeIt {
				expected[spec.State.String()]++
			}
		}
	}
	actual := map[string]int{}
	total := 0
	for _, suite := range junit.TestSuites {
		if suite.Tests != len(suite.TestCases) {
			return reports, counts, errors.New("inconsistent JUnit suite count")
		}
		total += len(suite.TestCases)
		for _, spec := range suite.TestCases {
			if strings.HasPrefix(spec.Name, "[It]") {
				actual[spec.Status]++
			}
		}
	}
	if total == 0 || total > maxNativeSpecs || total != junit.Tests || len(actual) != len(expected) {
		return reports, counts, errors.New("inconsistent JUnit report")
	}
	for state, count := range expected {
		if actual[state] != count {
			return reports, counts, fmt.Errorf("JSON/JUnit spec states disagree")
		}
	}
	return reports, counts, nil
}

func atomicWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".report-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func persistOutcome(dir string, outcome runOutcome) error {
	data, err := json.MarshalIndent(outcome, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, "outcome.json"), append(data, '\n'))
}

func (o runOutcome) validate(expected e2eci.Frozen) error {
	if err := expected.Validate(); err != nil {
		return err
	}
	_, resolved, err := preparedSelection(expected.Dispatch.Preset)
	if err != nil {
		return err
	}
	if o.Schema != 1 || o.Source != expected || o.Requested != expected.Dispatch.Preset || o.Resolved != resolved || o.Workers != preparedWorkers || o.Started.IsZero() || !o.Finished.IsZero() && o.Finished.Before(o.Started) {
		return errors.New("invalid outcome attribution")
	}
	for _, state := range []outcomeState{o.Infrastructure, o.Tests, o.Reports, o.Cleanup, o.Diagnostics, o.Artifact, o.Publication} {
		switch state {
		case stateNotRun, stateRunning, statePassed, stateFailed, stateCancelled, stateIncomplete, stateUnavailable:
		default:
			return errors.New("invalid lifecycle state")
		}
	}
	if o.Counts.Executed < 0 || o.Counts.Passed < 0 || o.Counts.Failed < 0 || o.Counts.Skipped < 0 || o.Counts.Executed != o.Counts.Passed+o.Counts.Failed || o.Counts.Executed+o.Counts.Skipped > maxNativeSpecs {
		return errors.New("invalid outcome counts")
	}
	if o.Tests == statePassed && (o.Finished.IsZero() || o.Reports != statePassed || o.Infrastructure != statePassed || o.Counts.Executed == 0 || o.Counts.Failed != 0) {
		return errors.New("passed tests lack successful native evidence")
	}
	if o.Infrastructure == statePassed && (o.ControllerCommit != expected.HeadSHA || !preparedImagePattern.MatchString(o.Image)) {
		return errors.New("controller identity differs from prepared source")
	}
	return nil
}

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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	e2eci "github.com/cloudpilot-ai/karpenter-provider-gcp/hack/e2e-ci"
)

const preparedWorkers = 20

func preparedSelection(preset string) (filter, resolved string, err error) {
	switch preset {
	case "standard", "gpu":
		resolved = preset
	case "full":
		resolved = "all"
	default:
		return "", "", fmt.Errorf("unsupported prepared preset")
	}
	filter, err = selectFilter("", resolved)
	return filter, resolved, err
}

// preparedExecution is a lock-held test-only seam, not a CI lifecycle. Its caller
// must VerifyDownload before auth, recheck authorization/target, acquire the Lease,
// deploy the prepared charts/image, and retain ownership through final persistence.
// Bundle and Source must arrive independently; no trusted tools come from Bundle.
type preparedExecution struct {
	Bundle        *e2eci.Bundle
	Source        e2eci.Frozen
	Image         string
	Workers       int
	OutputDir     string
	Environment   []string
	outputCreated bool
}

func validatePreparedExecution(input preparedExecution) error {
	if err := input.Source.Validate(); err != nil {
		return err
	}
	if input.Workers != preparedWorkers {
		return fmt.Errorf("prepared workers must equal %d", preparedWorkers)
	}
	b := input.Bundle
	if b == nil || b.Manifest.Source != input.Source || b.Manifest.Schema != 1 || b.Manifest.Platform != "linux/amd64" || b.Manifest.Outcome != "success" {
		return fmt.Errorf("prepared bundle does not match frozen source")
	}
	allowed := map[string]bool{e2eci.ControllerArchive: true, e2eci.ControllerChart: true, e2eci.CRDChart: true, e2eci.TestBinary: true, e2eci.GinkgoBinary: true}
	if len(b.Manifest.Files) != len(allowed) {
		return fmt.Errorf("incomplete prepared manifest")
	}
	for _, expected := range b.Manifest.Files {
		if !allowed[expected.Name] {
			return fmt.Errorf("unexpected or duplicate prepared file")
		}
		delete(allowed, expected.Name)
		path := filepath.Join(b.Root, expected.Name)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() != expected.Size || info.Size() <= 0 || info.Size() > e2eci.MaxFileBytes {
			return fmt.Errorf("prepared file changed")
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		h := sha256.New()
		n, readErr := io.Copy(h, io.LimitReader(f, e2eci.MaxFileBytes+1))
		err = errors.Join(readErr, f.Close())
		if err != nil {
			return err
		}
		if n != expected.Size || hex.EncodeToString(h.Sum(nil)) != expected.SHA256 {
			return fmt.Errorf("prepared file digest changed")
		}
	}
	layout, digest, err := b.ExtractControllerLayout("")
	if err != nil {
		return err
	}
	defer os.RemoveAll(layout)
	if !strings.HasSuffix(input.Image, "@"+digest) {
		return fmt.Errorf("requested controller digest differs from bundle")
	}
	return nil
}

func preparedEnvironment() []string {
	var env []string
	// No ambient GitHub token, compiler configuration or PR-defined subprocess env.
	for _, key := range []string{"PATH", "HOME", "TMPDIR", "KUBECONFIG", "GOOGLE_APPLICATION_CREDENTIALS", "CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE", "CLOUDSDK_CONFIG", "USE_GKE_GCLOUD_AUTH_PLUGIN", "PROJECT_ID", "CLUSTER_NAME", "CLUSTER_LOCATION", "PODS_RANGE_NAME", "SMALL_PODS_RANGE_NAME", "LARGE_PODS_RANGE_NAME", "RESOURCE_MANAGER_TAG_KEY", "E2E_Z3_TESTS", "E2E_Z4D_TESTS", "E2E_CONFIDENTIAL_TYPES"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func executePrepared(ctx context.Context, input preparedExecution) (outcome runOutcome, retErr error) {
	_, resolved, selectionErr := preparedSelection(input.Source.Dispatch.Preset)
	outcome = runOutcome{Schema: 1, Source: input.Source, Requested: input.Source.Dispatch.Preset, Resolved: resolved, Workers: input.Workers, Started: time.Now().UTC(), Image: input.Image,
		Infrastructure: stateNotRun, Tests: stateNotRun, Reports: stateNotRun, Cleanup: stateNotRun, Diagnostics: stateNotRun, Artifact: stateNotRun, Publication: stateNotRun}
	// A fresh private directory prevents stale native results from passing a run.
	if err := prepareOutputDirectory(input); err != nil {
		return outcome, err
	}
	output, err := filepath.Abs(input.OutputDir)
	if err != nil {
		return outcome, err
	}
	defer func() {
		outcome.Finished = time.Now().UTC()
		retErr = errors.Join(retErr, persistOutcome(output, outcome))
		// Even missing/corrupt native reports have a truthful rendered lifecycle.
		retErr = errors.Join(retErr, renderPreparedReport(output, outcome))
	}()
	if err := persistOutcome(output, outcome); err != nil {
		return outcome, err
	}
	if selectionErr != nil {
		outcome.Infrastructure = stateFailed
		return outcome, selectionErr
	}
	if err := ctx.Err(); err != nil {
		outcome.Tests = stateCancelled
		return outcome, err
	}
	if err := validatePreparedExecution(input); err != nil {
		outcome.Infrastructure = stateFailed
		return outcome, err
	}
	if err := verifyPreparedController(ctx, input.Image, input.Source.HeadSHA); err != nil {
		outcome.Infrastructure = stateFailed
		if ctx.Err() != nil {
			outcome.Infrastructure = stateCancelled
		}
		return outcome, err
	}
	outcome.Infrastructure = statePassed
	outcome.ControllerCommit = input.Source.HeadSHA
	outcome.Tests = stateRunning
	if err := persistOutcome(output, outcome); err != nil {
		return outcome, err
	}
	log, err := os.OpenFile(filepath.Join(output, "suite.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return outcome, err
	}
	filter, _, _ := preparedSelection(input.Source.Dispatch.Preset)
	args := []string{"--procs=" + strconv.Itoa(input.Workers), "--fail-on-empty", "--output-dir=" + output, "--json-report=results.json", "--junit-report=junit.xml"}
	if filter != "" {
		args = append(args, "--label-filter="+filter)
	}
	binary, err := filepath.Abs(filepath.Join(input.Bundle.Root, e2eci.TestBinary))
	if err != nil {
		log.Close()
		return outcome, err
	}
	args = append(args, binary)
	cli, err := filepath.Abs(filepath.Join(input.Bundle.Root, e2eci.GinkgoBinary))
	if err != nil {
		log.Close()
		return outcome, err
	}
	command := exec.CommandContext(ctx, cli, args...)
	command.Env = input.Environment
	if command.Env == nil {
		command.Env = preparedEnvironment()
	}
	command.Dir = output
	// os/exec serializes writes when stdout and stderr share the same writer.
	sink := &limitedWriter{Writer: log, Remaining: 32 << 20}
	command.Stdout, command.Stderr = sink, sink
	runErr := errors.Join(runIsolated(command), log.Close())
	reports, counts, reportErr := readNativeReports(output)
	outcome.Counts = counts
	outcome.Reports = statePassed
	if reportErr != nil {
		outcome.Reports = stateFailed
	}
	outcome.Tests = statePassed
	switch {
	case ctx.Err() != nil:
		outcome.Tests = stateCancelled
	case runErr != nil:
		outcome.Tests = stateFailed
	case reportErr != nil:
		outcome.Tests = stateIncomplete
	default:
		for _, report := range reports {
			if !report.SuiteSucceeded {
				outcome.Tests = stateFailed
			}
		}
		if counts.Failed > 0 {
			outcome.Tests = stateFailed
		}
	}
	if outcome.Tests != statePassed && runErr == nil {
		runErr = fmt.Errorf("prepared tests %s", outcome.Tests)
	}
	return outcome, errors.Join(runErr, reportErr)
}

func prepareOutputDirectory(input preparedExecution) error {
	if input.outputCreated {
		return nil
	}
	return os.Mkdir(input.OutputDir, 0700)
}

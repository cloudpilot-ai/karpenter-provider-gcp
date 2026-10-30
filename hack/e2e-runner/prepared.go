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
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const preparedWorkers = 20

type preparedExecution struct {
	SourceSHA, Selection                                                   string
	ControllerArchive, ControllerChart, CRDChart, TestBinary, GinkgoBinary string
	OutputDir                                                              string
}

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

func newPreparedCommand() *cobra.Command {
	var input preparedExecution
	var id string
	cmd := &cobra.Command{Use: "prepared", Short: "Run prepared artifacts under the canonical target Lease", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		target, err := loadLifecycleTarget(true)
		if err != nil {
			return err
		}
		ns, dep := controllerTarget()
		if ns != "karpenter-system" || dep != "karpenter" {
			return fmt.Errorf("prepared controller target override forbidden")
		}
		holder, err := resolveLockID(id)
		if err != nil {
			return err
		}
		client, err := newLeaseClient()
		if err != nil {
			return err
		}
		return runPreparedLifecycle(cmd.Context(), client, holder, target, input)
	}}
	for _, flag := range []struct {
		name  string
		value *string
	}{
		{"source-sha", &input.SourceSHA}, {"selection", &input.Selection},
		{"controller-archive", &input.ControllerArchive}, {"controller-chart", &input.ControllerChart}, {"crd-chart", &input.CRDChart},
		{"test-binary", &input.TestBinary}, {"ginkgo-binary", &input.GinkgoBinary}, {"results-dir", &input.OutputDir}, {"lock-id", &id},
	} {
		cmd.Flags().StringVar(flag.value, flag.name, "", flag.name)
		_ = cmd.MarkFlagRequired(flag.name)
	}
	return cmd
}

func validatePreparedExecution(input *preparedExecution) error {
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(input.SourceSHA) {
		return fmt.Errorf("invalid source SHA")
	}
	if _, _, err := preparedSelection(input.Selection); err != nil {
		return err
	}
	for _, path := range []*string{&input.ControllerArchive, &input.ControllerChart, &input.CRDChart, &input.TestBinary, &input.GinkgoBinary, &input.OutputDir} {
		if *path == "" {
			return fmt.Errorf("missing prepared path")
		}
		absolute, err := filepath.Abs(*path)
		if err != nil {
			return err
		}
		*path = absolute
	}
	for _, path := range []string{input.ControllerArchive, input.ControllerChart, input.CRDChart, input.TestBinary, input.GinkgoBinary} {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() <= 0 {
			return fmt.Errorf("invalid prepared file")
		}
	}
	return isolatedRuntimePaths(*input)
}

// Only tool/credential environment and explicitly validated target inputs cross
// the runtime boundary; no ambient GitHub tokens or compiler configuration.
func preparedEnvironment() []string {
	var env []string
	for _, key := range strings.Fields("PATH HOME TMPDIR KUBECONFIG GOOGLE_APPLICATION_CREDENTIALS CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE CLOUDSDK_CONFIG USE_GKE_GCLOUD_AUTH_PLUGIN PROJECT_ID CLUSTER_NAME CLUSTER_LOCATION PODS_RANGE_NAME SMALL_PODS_RANGE_NAME LARGE_PODS_RANGE_NAME RESOURCE_MANAGER_TAG_KEY E2E_Z3_TESTS E2E_Z4D_TESTS E2E_CONFIDENTIAL_TYPES") {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func (t lifecycleTarget) runtimeEnvironment() []string {
	return append(t.testEnvironment(),
		"E2E_PROJECT_ID="+t.Project, "E2E_LOCATION="+t.Location, "E2E_REGION="+t.Region, "E2E_PREFIX="+t.Prefix,
		"E2E_CLUSTER_NAME="+t.Cluster, "E2E_IMAGE_REPOSITORY="+t.Repository, "E2E_CONTROLLER_SERVICE_ACCOUNT="+t.ControllerSA,
		"E2E_NODE_SERVICE_ACCOUNT="+t.NodeSA, "E2E_PODS_RANGE_NAME="+t.Pods, "E2E_SMALL_PODS_RANGE_NAME="+t.SmallPods,
		"E2E_LARGE_PODS_RANGE_NAME="+t.LargePods, "E2E_RESOURCE_MANAGER_TAG_KEY="+t.TagKey)
}

func (t lifecycleTarget) testEnvironment() []string {
	values := map[string]string{"PROJECT_ID": t.Project, "CLUSTER_LOCATION": t.Location, "CLUSTER_NAME": t.Cluster, "PODS_RANGE_NAME": t.Pods, "SMALL_PODS_RANGE_NAME": t.SmallPods, "LARGE_PODS_RANGE_NAME": t.LargePods, "RESOURCE_MANAGER_TAG_KEY": t.TagKey}
	var env []string
	for _, item := range preparedEnvironment() {
		key, _, _ := strings.Cut(item, "=")
		if _, replace := values[key]; !replace {
			env = append(env, item)
		}
	}
	for key, value := range values {
		env = append(env, key+"="+value)
	}
	return env
}

func runPreparedLifecycle(ctx context.Context, client leaseClient, id string, target lifecycleTarget, input preparedExecution) (retErr error) {
	if err := validatePreparedExecution(&input); err != nil {
		return err
	}
	if err := os.Mkdir(input.OutputDir, 0700); err != nil {
		return err
	}
	// One final neutral verdict, including acquisition/cleanup failures. Job exit
	// independently fences release errors, even after passing tests and cleanup.
	verdict := struct {
		SourceSHA string `json:"source_sha"`
		TestExit  *int   `json:"test_exit"`
		CleanupOK bool   `json:"cleanup_ok"`
	}{SourceSHA: input.SourceSHA}
	defer func() {
		data, err := json.Marshal(verdict)
		if err == nil {
			err = writePrivateFile(filepath.Join(input.OutputDir, "runtime.json"), append(data, '\n'))
		}
		retErr = errors.Join(retErr, err)
	}()
	root, err := boundedCommandOutput(ctx, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	checkout := strings.TrimSpace(string(root))
	check, stop := context.WithTimeout(ctx, 2*time.Minute)
	err = target.preflight(check, true)
	stop()
	if err != nil {
		return err
	}
	return withOwnedLease(ctx, client, id, true, func(run context.Context, owner *leaseOwner) (result error) {
		mayCleanup := false
		defer func() {
			diag, cancel := context.WithTimeout(context.Background(), time.Minute)
			logs, logErr := boundedCommandOutput(diag, "kubectl", "-n", "karpenter-system", "logs", "deployment/karpenter", "--all-pods=true", "--all-containers=true", "--timestamps=true", "--tail=1000", "--limit-bytes=1048576")
			cancel()
			if logErr == nil {
				result = errors.Join(result, writePrivateFile(filepath.Join(input.OutputDir, "controller.log"), logs))
			}
			if mayCleanup {
				cleanup, stop := owner.cleanupContext(10 * time.Minute)
				cleanErr := cleanLocalEnvironment(cleanup, owner, target, checkout, target.runtimeEnvironment())
				stop()
				verdict.CleanupOK = cleanErr == nil
				result = errors.Join(result, cleanErr)
			}
			if !verdict.CleanupOK {
				mark, stop := owner.cleanupContext(10 * time.Second)
				result = errors.Join(result, owner.requireMaintenance(mark))
				stop()
			}
			if cause := context.Cause(owner.ctx); cause != nil {
				verdict.CleanupOK = false
				result = errors.Join(result, cause)
			}
		}()
		cleanup, stop := context.WithTimeout(run, 10*time.Minute)
		err := cleanLocalEnvironment(cleanup, owner, target, checkout, target.runtimeEnvironment())
		stop()
		if err != nil {
			return err
		}
		mayCleanup = true
		if err := owner.confirm(run); err != nil {
			return err
		}
		deploy := exec.CommandContext(run, "bash", filepath.Join(checkout, ".github/scripts/e2e/runtime.sh"), "deploy", input.ControllerArchive, input.ControllerChart, input.CRDChart, input.SourceSHA, input.OutputDir)
		deploy.Dir, deploy.Env = checkout, target.runtimeEnvironment()
		sink := &limitedWriter{Writer: io.Discard, Remaining: 2 << 20}
		deploy.Stdout, deploy.Stderr = sink, sink
		if err := runIsolated(deploy); err != nil {
			return fmt.Errorf("prepared deployment: %w", err)
		}
		data, err := readBoundedRegular(filepath.Join(input.OutputDir, "controller-image.txt"), 4096)
		if err != nil {
			return err
		}
		image := strings.TrimSuffix(string(data), "\n")
		if !preparedImagePattern.MatchString(image) || !strings.HasPrefix(image, target.Repository+"@sha256:") {
			return fmt.Errorf("prepared image differs from target repository/digest")
		}
		if err := verifyPreparedController(run, image, input.SourceSHA); err != nil {
			return err
		}
		if err := verifyTestBootstrap(run, target); err != nil {
			return err
		}
		if err := owner.confirm(run); err != nil {
			return err
		}
		verdict.TestExit, err = executePrepared(run, input, target.testEnvironment())
		return err
	})
}

func executePrepared(ctx context.Context, input preparedExecution, environment []string) (*int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	log, err := os.OpenFile(filepath.Join(input.OutputDir, "suite.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	filter, _, err := preparedSelection(input.Selection)
	if err != nil {
		return nil, errors.Join(err, log.Close())
	}
	args := []string{"--procs=" + strconv.Itoa(preparedWorkers), "--fail-on-empty", "--output-dir=" + input.OutputDir, "--json-report=results.json", "--junit-report=junit.xml"}
	if filter != "" {
		args = append(args, "--label-filter="+filter)
	}
	args = append(args, input.TestBinary)
	command := exec.CommandContext(ctx, input.GinkgoBinary, args...)
	command.Dir, command.Env = input.OutputDir, environment
	sink := &limitedWriter{Writer: log, Remaining: 32 << 20}
	command.Stdout, command.Stderr = sink, sink
	runErr := runIsolated(command)
	var exit *int
	if command.ProcessState != nil {
		code := command.ProcessState.ExitCode()
		if code < 0 {
			code = 1
		}
		exit = &code
	}
	runErr = errors.Join(runErr, log.Close())
	reports, counts, reportErr := readJSONReports(filepath.Join(input.OutputDir, "results.json"))
	for _, report := range reports {
		if !report.SuiteSucceeded {
			reportErr = errors.Join(reportErr, fmt.Errorf("native suite failed"))
		}
	}
	if counts.Failed > 0 {
		reportErr = errors.Join(reportErr, fmt.Errorf("native specs failed"))
	}
	return exit, errors.Join(runErr, reportErr, ctx.Err())
}

func isolatedRuntimePaths(input preparedExecution) error {
	output, err := canonicalLocation(input.OutputDir)
	if err != nil {
		return err
	}
	roots := []string{output}
	for _, path := range []string{input.ControllerArchive, input.ControllerChart, input.CRDChart, input.TestBinary, input.GinkgoBinary} {
		root, err := canonicalLocation(filepath.Dir(path))
		if err != nil {
			return err
		}
		if pathsOverlap(root, output) {
			return fmt.Errorf("artifact/results roots overlap")
		}
		roots = append(roots, root)
	}
	for _, key := range []string{"GOOGLE_APPLICATION_CREDENTIALS", "CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE", "GOOGLE_GHA_CREDS_PATH", "KUBECONFIG", "CLOUDSDK_CONFIG", "TMPDIR"} {
		value := os.Getenv(key)
		if value == "" {
			continue
		}
		paths := []string{value}
		if key == "KUBECONFIG" {
			paths = filepath.SplitList(value)
		}
		for _, path := range paths {
			cleanPath, err := canonicalLocation(path)
			if err != nil {
				return err
			}
			for _, root := range roots {
				relative, err := filepath.Rel(root, cleanPath)
				if err != nil {
					return err
				}
				inside := relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
				if inside || key != "TMPDIR" && pathsOverlap(cleanPath, root) {
					return fmt.Errorf("runtime credential/config path overlaps artifact/results root")
				}
			}
		}
	}
	return nil
}

func canonicalLocation(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(absolute)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(absolute)
		if parent == absolute {
			return "", err
		}
		suffix = append(suffix, filepath.Base(absolute))
		absolute = parent
	}
}

func pathsOverlap(a, b string) bool {
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		relative, err := filepath.Rel(pair[0], pair[1])
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

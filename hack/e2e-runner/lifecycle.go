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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	e2eci "github.com/cloudpilot-ai/karpenter-provider-gcp/hack/e2e-ci"
)

func lifecycleOutcome(input preparedExecution) runOutcome {
	_, resolved, _ := preparedSelection(input.Source.Dispatch.Preset)
	return runOutcome{Schema: 1, Source: input.Source, Requested: input.Source.Dispatch.Preset, Resolved: resolved, Workers: input.Workers, Image: input.Image, Started: time.Now().UTC(), Infrastructure: stateNotRun, Tests: stateNotRun, Reports: stateNotRun, Cleanup: stateNotRun, Diagnostics: stateNotRun, Artifact: stateNotRun, Publication: stateNotRun}
}

func (t lifecycleTarget) testEnvironment() []string {
	env := preparedEnvironment()
	values := map[string]string{"PROJECT_ID": t.Project, "CLUSTER_LOCATION": t.Location, "CLUSTER_NAME": t.Cluster, "PODS_RANGE_NAME": t.Pods, "SMALL_PODS_RANGE_NAME": t.SmallPods, "LARGE_PODS_RANGE_NAME": t.LargePods, "RESOURCE_MANAGER_TAG_KEY": t.TagKey}
	var result []string
	for _, item := range env {
		key, _, _ := strings.Cut(item, "=")
		if _, replace := values[key]; !replace {
			result = append(result, item)
		}
	}
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	return result
}

func runPreparedLifecycle(ctx context.Context, client leaseClient, id string, target lifecycleTarget, input preparedExecution) (retErr error) {
	if err := os.Mkdir(input.OutputDir, 0700); err != nil {
		return err
	}
	outcome := lifecycleOutcome(input)
	save := func() error {
		if outcome.Cleanup == stateIncomplete {
			outcome.MaintenanceRequired = true
		}
		outcome.Finished = time.Now().UTC()
		return errors.Join(persistOutcome(input.OutputDir, outcome), renderPreparedReport(input.OutputDir, outcome))
	}
	if err := persistOutcome(input.OutputDir, outcome); err != nil {
		return err
	}
	if err := validatePreparedExecution(input); err != nil {
		outcome.Infrastructure = stateFailed
		return errors.Join(err, save())
	}
	if err := isolatedRuntimePaths(input); err != nil {
		outcome.Infrastructure = stateFailed
		return errors.Join(err, save())
	}
	if !strings.HasPrefix(input.Image, target.Repository+"@") {
		outcome.Infrastructure = stateFailed
		return errors.Join(fmt.Errorf("prepared image repository differs from fixed target"), save())
	}
	check, stop := context.WithTimeout(ctx, 2*time.Minute)
	checkErr := target.preflight(check, true)
	stop()
	if err := checkErr; err != nil {
		outcome.Infrastructure = stateFailed
		return errors.Join(err, save())
	}
	entered := false
	err := withOwnedLease(ctx, client, id, true, func(run context.Context, owner *leaseOwner) (result error) {
		entered = true
		mayCleanup := false
		defer func() {
			// Read-only diagnostics are permitted even after ownership uncertainty.
			diag, cancel := context.WithTimeout(context.Background(), time.Minute)
			logs, logErr := boundedCommandOutput(diag, "kubectl", "-n", "karpenter-system", "logs", "deployment/karpenter", "--all-pods=true", "--all-containers=true", "--timestamps=true", "--tail=1000", "--limit-bytes=1048576")
			cancel()
			outcome.Diagnostics = statePassed
			if logErr != nil {
				outcome.Diagnostics = stateUnavailable
			} else if err := atomicWrite(filepath.Join(input.OutputDir, "controller.log"), logs); err != nil {
				outcome.Diagnostics = stateFailed
				result = errors.Join(result, err)
			}
			if mayCleanup {
				cleanup, stop := owner.cleanupContext(10 * time.Minute)
				cleanErr := owner.confirm(cleanup)
				if cleanErr == nil {
					cleanErr = drainOwned(cleanup, owner, target)
				}
				stop()
				outcome.Cleanup = statePassed
				if cleanErr != nil {
					outcome.Cleanup = stateIncomplete
					result = errors.Join(result, cleanErr)
				}
			}
			if outcome.Cleanup == stateIncomplete && context.Cause(owner.ctx) == nil {
				mark, stop := owner.cleanupContext(10 * time.Second)
				result = errors.Join(result, owner.requireMaintenance(mark))
				stop()
			}
			if context.Cause(owner.ctx) != nil {
				outcome.Cleanup = stateIncomplete
				result = errors.Join(result, context.Cause(owner.ctx))
			}
			result = errors.Join(result, save())
		}()
		// Drain before pushing/upgrading, while the old controller is still present.
		cleanup, stop := context.WithTimeout(run, 10*time.Minute)
		err := drainOwned(cleanup, owner, target)
		stop()
		if err != nil {
			outcome.Infrastructure = stateFailed
			outcome.Cleanup = stateIncomplete
			return err
		}
		mayCleanup = true
		image, err := pushPreparedController(run, owner, target, input.Bundle, input.Source)
		if err != nil {
			outcome.Infrastructure = stateFailed
			return err
		}
		if image != input.Image {
			outcome.Infrastructure = stateFailed
			return fmt.Errorf("uploaded image identity mismatch")
		}
		if err := deployPrepared(run, owner, target, input); err != nil {
			outcome.Infrastructure = stateFailed
			return err
		}
		if err := verifyTestBootstrap(run, target); err != nil {
			outcome.Infrastructure = stateFailed
			return err
		}
		input.Environment = target.testEnvironment()
		input.outputCreated = true
		outcome, err = executePrepared(run, input)
		if err != nil && run.Err() != nil && outcome.Tests == stateNotRun {
			outcome.Tests = stateCancelled
		}
		return err
	})
	// On acquisition failure there was no ownership; only persist local evidence.
	if !entered {
		outcome.MaintenanceRequired = errors.Is(err, errTargetMaintenance)
		outcome.Infrastructure = stateFailed
		err = errors.Join(err, save())
	} else if errors.Is(err, errLeaseUncertain) {
		outcome.Cleanup = stateIncomplete
		err = errors.Join(err, save())
	}
	return err
}

func pushPreparedController(ctx context.Context, owner *leaseOwner, t lifecycleTarget, b *e2eci.Bundle, source e2eci.Frozen) (string, error) {
	layout, digest, err := b.ExtractControllerLayout("")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(layout)

	// A private helper configuration uses refreshable gcloud WIF credentials;
	// never persist an access token or change the operator's Docker config.
	if _, err := exec.LookPath("docker-credential-gcloud"); err != nil {
		return "", err
	}
	authDir, err := os.MkdirTemp("", "e2e-registry-auth-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(authDir)
	host, _, ok := strings.Cut(t.Repository, "/")
	if !ok {
		return "", fmt.Errorf("invalid registry repository")
	}
	config, err := json.Marshal(map[string]any{"credHelpers": map[string]string{host: "gcloud"}})
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(authDir, "config.json"), config, 0600); err != nil {
		return "", err
	}
	env := append(preparedEnvironment(), "DOCKER_CONFIG="+authDir)
	tag := t.Repository + ":e2e-" + source.HeadSHA + "-" + strconv.FormatInt(source.Dispatch.RunID, 10)
	if err := owner.confirm(ctx); err != nil {
		return "", err
	}
	push := exec.CommandContext(ctx, "crane", "push", layout, tag)
	push.Env = env
	push.Stdout = &limitedWriter{Writer: io.Discard, Remaining: 2 << 20}
	push.Stderr = &limitedWriter{Writer: io.Discard, Remaining: 2 << 20}
	if err := runIsolated(push); err != nil {
		return "", err
	}
	var actual bytes.Buffer
	read := exec.CommandContext(ctx, "crane", "digest", tag)
	read.Env = env
	read.Stdout = &limitedWriter{Writer: &actual, Remaining: 4096}
	if err := runIsolated(read); err != nil {
		return "", err
	}

	if strings.TrimSpace(actual.String()) != digest {
		return "", fmt.Errorf("registry digest differs from prepared image")
	}
	return t.Repository + "@" + digest, nil
}

func deployPrepared(ctx context.Context, owner *leaseOwner, t lifecycleTarget, input preparedExecution) error {
	// CI only checks bootstrap resources. It never installs dependencies, creates
	// controller namespaces/priority classes, builds source or executes PR scripts.

	if err := mutation(ctx, owner, "helm", "upgrade", "--install", "karpenter-crd", filepath.Join(input.Bundle.Root, e2eci.CRDChart), "--namespace", "karpenter-system", "--wait", "--timeout", "2m"); err != nil {
		return err
	}
	args := []string{"upgrade", "--install", "karpenter", filepath.Join(input.Bundle.Root, e2eci.ControllerChart), "--namespace", "karpenter-system", "--wait", "--timeout", "5m"}
	values := []string{
		"logLevel=debug", "controller.settings.projectID=" + t.Project, "controller.settings.clusterName=" + t.Cluster, "controller.settings.clusterLocation=" + t.Location,
		"controller.featureGates.spotToSpotConsolidation=true", "controller.featureGates.nodeRepair=true", "controller.featureGates.nodeOverlay=true", "controller.featureGates.staticCapacity=true",
		`serviceAccount.annotations.iam\.gke\.io/gcp-service-account=` + t.ControllerSA, "controller.replicaCount=1", "credentials.enabled=false",
		"controller.settings.defaultNodepoolServiceAccount=" + t.NodeSA, "controller.image.repository=" + t.Repository, "controller.image.tag=prepared", "controller.image.digest=" + strings.SplitN(input.Image, "@", 2)[1],
	}
	for _, value := range values {
		args = append(args, "--set-string", value)
	}
	// Helm's --set-string cannot be used for boolean/number values in typed charts.
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--set-string" {
			v := args[i+1]
			if strings.HasSuffix(v, "=true") || strings.HasSuffix(v, "=false") || v == "controller.replicaCount=1" {
				args[i] = "--set"
			}
		}
	}
	return mutation(ctx, owner, "helm", args...)
}

func newPreparedCommand() *cobra.Command {
	var sourcePath, receiptPath, download, output, id string
	cmd := &cobra.Command{Use: "prepared", Short: "Trusted prepared CI lifecycle (pre-auth verification and authorization recheck required)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		sourceData, err := readBoundedRegular(sourcePath, e2eci.MaxMetadataBytes)
		if err != nil {
			return err
		}
		var source e2eci.Frozen
		if err := e2eci.DecodeStrict(strings.NewReader(string(sourceData)), e2eci.MaxMetadataBytes, &source); err != nil {
			return err
		}
		receiptData, err := readBoundedRegular(receiptPath, e2eci.MaxMetadataBytes)
		if err != nil {
			return err
		}
		var receipt e2eci.Receipt
		if err := e2eci.DecodeStrict(strings.NewReader(string(receiptData)), e2eci.MaxMetadataBytes, &receipt); err != nil {
			return err
		}
		bundle, err := e2eci.VerifyDownload(download, "", source, receipt)
		if err != nil {
			return err
		}
		defer os.RemoveAll(bundle.Root)
		target, err := loadLifecycleTarget(true)
		if err != nil {
			return err
		}
		// A runtime namespace/deployment override is not part of this fixed CI target.
		ns, dep := controllerTarget()
		if ns != "karpenter-system" || dep != "karpenter" {
			return fmt.Errorf("prepared controller target override forbidden")
		}
		layout, digest, err := bundle.ExtractControllerLayout("")
		if err != nil {
			return err
		}
		os.RemoveAll(layout)
		holder, err := resolveLockID(id)
		if err != nil {
			return err
		}
		client, err := newLeaseClient()
		if err != nil {
			return err
		}
		return runPreparedLifecycle(cmd.Context(), client, holder, target, preparedExecution{Bundle: bundle, Source: source, Image: target.Repository + "@" + digest, Workers: preparedWorkers, OutputDir: output})
	}}
	cmd.Flags().StringVar(&sourcePath, "source", "", "Independent trusted gate JSON")
	cmd.Flags().StringVar(&receiptPath, "receipt", "", "Independent trusted exact artifact receipt JSON")
	cmd.Flags().StringVar(&download, "download", "", "Original artifact ZIP (also verify before cloud authentication)")
	cmd.Flags().StringVar(&output, "results-dir", "", "New private result directory")
	cmd.Flags().StringVar(&id, "lock-id", "", "Lease holder ID")
	for _, key := range []string{"source", "receipt", "download", "results-dir"} {
		_ = cmd.MarkFlagRequired(key)
	}
	return cmd
}

func isolatedRuntimePaths(input preparedExecution) error {
	roots := []string{input.Bundle.Root, input.OutputDir}
	for _, key := range []string{"GOOGLE_APPLICATION_CREDENTIALS", "CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE", "GOOGLE_GHA_CREDS_PATH", "KUBECONFIG", "CLOUDSDK_CONFIG"} {
		value := os.Getenv(key)
		if value == "" {
			continue
		}
		paths := []string{value}
		if key == "KUBECONFIG" {
			paths = filepath.SplitList(value)
		}
		for _, path := range paths {
			for _, root := range roots {
				cleanPath, err := canonicalLocation(path)
				if err != nil {
					return err
				}
				cleanRoot, err := canonicalLocation(root)
				if err != nil {
					return err
				}
				relative, err := filepath.Rel(cleanRoot, cleanPath)
				if err != nil {
					return err
				}
				if relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
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

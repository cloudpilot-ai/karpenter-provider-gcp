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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

var presets = map[string]string{
	"standard":     "!suite:gpu",
	"gpu":          "suite:gpu",
	"all":          "",
	"provisioning": "suite:provisioning",
}

func selectFilter(dir, selection string) (string, error) {
	selection = strings.TrimSpace(selection)
	if selection == "" {
		return "", fmt.Errorf("e2e selection must not be empty")
	}
	if filter, ok := presets[selection]; ok {
		return filter, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	features := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			features[entry.Name()] = true
		}
	}
	var filters []string
	seen := map[string]bool{}
	for _, part := range strings.Split(selection, ",") {
		name := strings.TrimSpace(part)
		if !features[name] {
			return "", fmt.Errorf("unknown e2e feature %q in selection %q (use a preset or feature directory names)", name, selection)
		}
		if !seen[name] {
			filters = append(filters, "suite:"+name)
			seen[name] = true
		}
	}
	return strings.Join(filters, " || "), nil
}

func main() {
	var selection, preset, reportPath, lockID string
	cmd := &cobra.Command{
		Use:          "e2e-runner",
		Short:        "Run e2e features by preset or directory name",
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, ginkgoArgs []string) error {
			if cmd.Flags().Changed("preset") {
				if cmd.Flags().Changed("selection") {
					return fmt.Errorf("--selection and --preset cannot be used together")
				}
				selection = preset
			}
			filter, err := selectFilter("test/suites", selection)
			if err != nil {
				return err
			}
			client, err := newLeaseClient()
			if err != nil {
				return err
			}
			return withLease(cmd.Context(), client, lockID, func(runCtx context.Context) error {
				args := append([]string{"run", "github.com/onsi/ginkgo/v2/ginkgo"}, ginkgoArgs...)
				if filter != "" {
					args = append(args, "--label-filter="+filter)
				}
				var jsonPath string
				var metadata reportMetadata
				if reportPath != "" {
					if err := os.Remove(reportPath); err != nil && !os.IsNotExist(err) {
						return err
					}
					metadata.Commit, metadata.ControllerCommit, err = verifyControllerCommit(runCtx)
					if err != nil {
						return err
					}
					dir, err := os.MkdirTemp("", "e2e-report-")
					if err != nil {
						return err
					}
					defer os.RemoveAll(dir)
					jsonPath = filepath.Join(dir, "results.json")
					args = append(args, "--output-dir="+dir, "--json-report=results.json")
				}
				args = append(args, "./test/suites/")
				runner := exec.CommandContext(runCtx, "go", args...)
				runner.Stdin, runner.Stdout, runner.Stderr = os.Stdin, os.Stdout, os.Stderr
				start := time.Now()
				runErr := runner.Run()
				if reportPath != "" {
					metadata.Duration = time.Since(start).Round(time.Second).String()
					logPath := strings.TrimSuffix(reportPath, filepath.Ext(reportPath)) + ".karpenter.log"
					if err := dumpControllerLogs(runCtx, logPath, start); err != nil {
						metadata.LogsUnavailable = true
						fmt.Fprintf(os.Stderr, "controller logs unavailable: %v\n", err)
					} else {
						fmt.Fprintf(os.Stderr, "controller logs: %s\n", logPath)
					}
					return errors.Join(runErr, writeReport(jsonPath, reportPath, metadata))
				}
				return runErr
			})
		},
	}
	cmd.Flags().StringVar(&selection, "selection", "standard", "Preset (standard, gpu, all, provisioning) or comma-separated feature directories")
	cmd.Flags().StringVar(&preset, "preset", "", "Deprecated alias for --selection")
	cmd.Flags().StringVar(&reportPath, "report", "", "Write a Markdown test report to this path")
	cmd.Flags().StringVar(&lockID, "lock-id", "", "Lease holder ID (required; Make defaults to username@hostname:pid-<shell PID>)")
	if err := cmd.MarkFlagRequired("lock-id"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cmd.ExecuteContext(ctx); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.ExitCode())
		}
		os.Exit(1)
	}
}

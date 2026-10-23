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
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func newLocalLifecycleCommand(deploy bool) *cobra.Command {
	name := "local-clean-env"
	if deploy {
		name = "local-deploy"
	}
	var id string
	cmd := &cobra.Command{Use: name, Short: "Lease-owned explicit local/release lifecycle", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		target, err := loadLifecycleTarget(false)
		if err != nil {
			return err
		}
		check, stop := context.WithTimeout(cmd.Context(), 2*time.Minute)
		err = target.preflight(check, false)
		stop()
		if err != nil {
			return err
		}
		holder, err := resolveLockID(id)
		if err != nil {
			return err
		}
		client, err := newLeaseClient()
		if err != nil {
			return err
		}
		return withOwnedLease(cmd.Context(), client, holder, true, func(ctx context.Context, owner *leaseOwner) error {
			return runLocalLifecycle(ctx, owner, target, deploy)
		})
	}}
	cmd.Flags().StringVar(&id, "lock-id", "", "Lease holder ID")
	return cmd
}

// Local mutations remain in the original sourceable scripts. The coordinator
// holds one Lease across capture, Bash cleanup, cloud proof and Bash deployment.
func runLocalLifecycle(ctx context.Context, owner *leaseOwner, target lifecycleTarget, deploy bool) error {
	root, err := boundedCommandOutput(ctx, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	cleanup, stop := context.WithTimeout(ctx, 10*time.Minute)
	err = cleanLocalEnvironment(cleanup, owner, target, strings.TrimSpace(string(root)), nil)
	stop()
	if err != nil {
		// Cancellation never starts a second cleanup. Only confirmed ownership
		// permits marking the incomplete attempt; uncertainty prohibits writes.
		mark, stop := owner.cleanupContext(10 * time.Second)
		markErr := owner.requireMaintenance(mark)
		stop()
		return errors.Join(err, markErr)
	}
	if !deploy {
		return nil
	}
	if err := owner.confirm(ctx); err != nil {
		return err
	}
	shell := exec.CommandContext(ctx, "bash", "-e", "-c", `source "$1"; e2e_deploy`, "local-deploy", filepath.Join(strings.TrimSpace(string(root)), "hack/e2e-deploy.sh"))
	shell.Stdin, shell.Stdout, shell.Stderr = os.Stdin, os.Stdout, os.Stderr
	shell.Env = append(os.Environ(), "E2E_CLUSTER_NAME="+target.Cluster, "E2E_REGION="+target.Region, "E2E_CONTROLLER_SERVICE_ACCOUNT="+target.ControllerSA, "E2E_IMAGE_REPOSITORY="+target.Repository)
	if err := runIsolated(shell); err != nil {
		return fmt.Errorf("local deployment: %w", err)
	}
	return owner.confirm(ctx)
}

func cleanLocalEnvironment(ctx context.Context, owner *leaseOwner, target lifecycleTarget, root string, environment []string) error {
	inventory, err := inspectDrain(ctx, target)
	if err != nil {
		return err
	}
	if err := owner.confirm(ctx); err != nil {
		return err
	}
	args := []string{"-e", "-c", `source "$1"; shift; e2e_clean_env "$@"`, "local-clean-env", filepath.Join(root, "hack/e2e-clean-env.sh")}
	for _, deployment := range inventory.workloads["deployments.apps"] {
		args = append(args, deployment.Metadata.Name)
	}
	shell := exec.CommandContext(ctx, "bash", args...)
	shell.Stdin, shell.Stdout, shell.Stderr = os.Stdin, os.Stdout, os.Stderr
	shell.Env = environment
	if environment != nil {
		sink := &limitedWriter{Writer: io.Discard, Remaining: 2 << 20}
		shell.Stdout, shell.Stderr = sink, sink
	}

	if err := runIsolated(shell); err != nil {
		return fmt.Errorf("maintenance-required: local cleanup: %w", err)
	}
	return verifyDrained(ctx, owner, target, inventory)
}

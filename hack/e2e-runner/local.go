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
			drain, stop := context.WithTimeout(ctx, 10*time.Minute)
			err := drainOwned(drain, owner, target)
			stop()
			if err != nil {
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
			root, err := boundedCommandOutput(ctx, "git", "rev-parse", "--show-toplevel")
			if err != nil {
				return err
			}
			// Explicit local build/release mode only; prepared CI never enters this body.
			shell := exec.CommandContext(ctx, "bash", filepath.Join(strings.TrimSpace(string(root)), "hack/internal/e2e-deploy-held.sh"))
			shell.Stdin, shell.Stdout, shell.Stderr = os.Stdin, os.Stdout, os.Stderr
			shell.Env = append(os.Environ(), "E2E_CLUSTER_NAME="+target.Cluster, "E2E_CONTROLLER_SERVICE_ACCOUNT="+target.ControllerSA, "E2E_IMAGE_REPOSITORY="+target.Repository)
			if err := runIsolated(shell); err != nil {
				return fmt.Errorf("local deployment: %w", err)
			}
			return owner.confirm(ctx)
		})
	}}
	cmd.Flags().StringVar(&id, "lock-id", "", "Lease holder ID")
	return cmd
}

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
	"regexp"
	"strings"
	"time"
)

func controllerTarget() (string, string) {
	namespace := os.Getenv("KARPENTER_NAMESPACE")
	if namespace == "" {
		namespace = "karpenter-system"
	}
	deployment := os.Getenv("KARPENTER_DEPLOYMENT")
	if deployment == "" {
		deployment = "karpenter"
	}
	return namespace, deployment
}

func verifyControllerCommit(ctx context.Context) (string, string, error) {
	sha, err := exec.CommandContext(ctx, "git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "", "", fmt.Errorf("reading test commit: %w", err)
	}
	commit := strings.TrimSpace(string(sha))
	status, err := exec.CommandContext(ctx, "git", "status", "--porcelain").Output()
	if err != nil {
		return "", "", fmt.Errorf("checking test source: %w", err)
	}
	if len(status) > 0 {
		commit += "-dirty"
	}
	base := strings.TrimSuffix(commit, "-dirty")

	if project, location, cluster := os.Getenv("PROJECT_ID"), os.Getenv("CLUSTER_LOCATION"), os.Getenv("CLUSTER_NAME"); project != "" && location != "" && cluster != "" {
		current, err := exec.CommandContext(ctx, "kubectl", "config", "current-context").Output()
		if err != nil {
			return "", "", fmt.Errorf("checking cluster context: %w", err)
		}
		if expected := "gke_" + project + "_" + location + "_" + cluster; strings.TrimSpace(string(current)) != expected {
			return "", "", fmt.Errorf("cluster context %q does not match %q", strings.TrimSpace(string(current)), expected)
		}
	}

	namespace, deployment := controllerTarget()
	image, err := exec.CommandContext(ctx, "kubectl", "-n", namespace, "get", "deployment", deployment, "-o", "jsonpath={.spec.template.spec.containers[0].image}").Output()
	if err != nil {
		return "", "", fmt.Errorf("checking deployed controller image: %w", err)
	}
	tagged := strings.SplitN(strings.TrimSpace(string(image)), "@", 2)[0]
	if tag := tagged[strings.LastIndex(tagged, ":")+1:]; tag != "e2e-"+base {
		return "", "", fmt.Errorf("deployed controller image %q does not match test commit %s", strings.TrimSpace(string(image)), commit)
	}
	logs, err := exec.CommandContext(ctx, "kubectl", "-n", namespace, "logs", "deployment/"+deployment, "--tail=50").Output()
	if err != nil {
		return "", "", fmt.Errorf("checking controller binary commit: %w", err)
	}
	found := regexp.MustCompile(`"commit":"([0-9a-f]+)(-dirty)?"`).FindStringSubmatch(string(logs))
	if len(found) == 0 || !strings.HasPrefix(base, found[1]) && !strings.HasPrefix(found[1], base) {
		return "", "", fmt.Errorf("controller binary commit in logs does not match test commit %s", commit)
	}
	return commit, found[1] + found[2], nil
}

func dumpControllerLogs(ctx context.Context, path string, since time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	namespace, deployment := controllerTarget()
	command := exec.CommandContext(ctx, "kubectl", "-n", namespace, "logs", "deployment/"+deployment,
		"--all-pods=true", "--all-containers=true", "--timestamps=true", "--since-time="+since.UTC().Format(time.RFC3339))
	command.Stdout, command.Stderr = file, os.Stderr
	runErr := command.Run()
	closeErr := file.Close()
	if err := errors.Join(runErr, closeErr); err != nil {
		os.Remove(path)
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		os.Remove(path)
		return fmt.Errorf("no controller logs during test run")
	}
	return nil
}

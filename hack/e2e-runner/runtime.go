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
	"regexp"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func verifyControllerCommit(ctx context.Context, releaseVersion string) (string, string, error) {
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
	tag := tagged[strings.LastIndex(tagged, ":")+1:]
	if releaseVersion != "" {
		if tag != "v"+strings.TrimPrefix(releaseVersion, "v") {
			return "", "", fmt.Errorf("deployed controller image %q does not match requested release %s", strings.TrimSpace(string(image)), releaseVersion)
		}
	} else if tag != "e2e-"+base {
		return "", "", fmt.Errorf("deployed controller image %q does not match test commit %s", strings.TrimSpace(string(image)), commit)
	}
	logs, err := exec.CommandContext(ctx, "kubectl", "-n", namespace, "logs", "deployment/"+deployment, "--tail=50").Output()
	if err != nil {
		return "", "", fmt.Errorf("checking controller binary commit: %w", err)
	}
	found := regexp.MustCompile(`"commit":"([0-9a-f]+)(-dirty)?"`).FindStringSubmatch(string(logs))
	if len(found) == 0 {
		return "", "", fmt.Errorf("controller binary commit not found in logs")
	}
	if releaseVersion == "" && !strings.HasPrefix(base, found[1]) && !strings.HasPrefix(found[1], base) {
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

var preparedImagePattern = regexp.MustCompile(`^[^\s@]+@sha256:[a-f0-9]{64}$`)

// verifyPreparedController never consults runtime git. Both desired and running
// image identities and every ready controller's full embedded SHA must agree.
func verifyPreparedController(ctx context.Context, image, sha string) error {
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(sha) || !preparedImagePattern.MatchString(image) {
		return fmt.Errorf("invalid prepared controller identity")
	}
	namespace, deployment := controllerTarget()
	if _, err := boundedCommandOutput(ctx, "kubectl", "-n", namespace, "rollout", "status", "deployment/"+deployment, "--timeout=120s"); err != nil {
		return fmt.Errorf("controller rollout: %w", err)
	}
	data, err := boundedCommandOutput(ctx, "kubectl", "-n", namespace, "get", "deployment", deployment, "-o", "json")
	if err != nil {
		return err
	}
	var deployed appsv1.Deployment
	if err := json.Unmarshal(data, &deployed); err != nil {
		return err
	}
	if len(deployed.Spec.Template.Spec.Containers) != 1 || deployed.Spec.Template.Spec.Containers[0].Image != image || deployed.Spec.Selector == nil || deployed.Spec.Replicas == nil || *deployed.Spec.Replicas < 1 {
		return fmt.Errorf("deployment does not match prepared controller image")
	}
	container := deployed.Spec.Template.Spec.Containers[0].Name
	selector, err := metav1.LabelSelectorAsSelector(deployed.Spec.Selector)
	if err != nil || selector.Empty() {
		return fmt.Errorf("missing controller pod selector")
	}
	data, err = boundedCommandOutput(ctx, "kubectl", "-n", namespace, "get", "pods", "-l", selector.String(), "-o", "json")
	if err != nil {
		return err
	}
	var pods corev1.PodList
	if err := json.Unmarshal(data, &pods); err != nil {
		return err
	}
	if len(pods.Items) != int(*deployed.Spec.Replicas) {
		return fmt.Errorf("controller replica count does not match rollout")
	}
	digest := strings.SplitN(image, "@", 2)[1]
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp != nil {
			return fmt.Errorf("controller pod is terminating")
		}
		ready := false
		for _, spec := range pod.Spec.Containers {
			if spec.Name == container && spec.Image == image {
				for _, status := range pod.Status.ContainerStatuses {
					actual := status.ImageID
					if i := strings.LastIndex(actual, "@"); i >= 0 {
						actual = actual[i+1:]
					}
					actual = strings.TrimPrefix(actual, "containerd://")
					ready = ready || status.Name == container && status.Ready && actual == digest
				}
			}
		}
		if !ready {
			return fmt.Errorf("running controller digest is not the prepared image")
		}
		logs, err := boundedCommandOutput(ctx, "kubectl", "-n", namespace, "logs", pod.Name, "-c", container, "--tail=1000", "--limit-bytes=1048576")
		if err != nil {
			return err
		}
		found := false
		for _, line := range strings.Split(string(logs), "\n") {
			var entry struct {
				Commit string `json:"commit"`
			}
			if json.Unmarshal([]byte(line), &entry) == nil && entry.Commit != "" {
				if entry.Commit != sha {
					return fmt.Errorf("embedded controller SHA differs from prepared source")
				}
				found = true
			}
		}
		if !found {
			return fmt.Errorf("full embedded controller SHA unavailable")
		}
	}
	return nil
}

func boundedCommandOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	var output bytes.Buffer
	command := exec.CommandContext(ctx, name, args...)
	command.Stdout = &limitedWriter{Writer: &output, Remaining: 2 << 20}
	if err := runIsolated(command); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

type limitedWriter struct {
	io.Writer
	Remaining int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if len(p) > w.Remaining {
		return 0, fmt.Errorf("output limit exceeded")
	}
	n, err := w.Writer.Write(p)
	w.Remaining -= n
	return n, err
}

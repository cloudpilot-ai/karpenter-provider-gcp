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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestControllerCommitAndLogs(t *testing.T) {
	sha, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	kubectl := filepath.Join(dir, "kubectl")
	script := `#!/bin/sh
case "$*" in
  'config current-context') echo "${TEST_CONTEXT}" ;;
  *' get deployment '*) echo "${TEST_IMAGE}" ;;
  *' logs deployment/'*) if [ "${TEST_LOG_FAIL:-}" = 1 ]; then exit 1; fi; echo "{\"commit\":\"${TEST_EMBEDDED}\"}" ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(kubectl, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("PROJECT_ID", "project")
	t.Setenv("CLUSTER_LOCATION", "zone")
	t.Setenv("CLUSTER_NAME", "cluster")
	t.Setenv("TEST_CONTEXT", "gke_project_zone_cluster")
	t.Setenv("TEST_IMAGE", "registry/karpenter:e2e-"+strings.TrimSpace(string(sha))+"@sha256:abc")
	t.Setenv("TEST_EMBEDDED", strings.TrimSpace(string(sha)))
	commit, embedded, err := verifyControllerCommit(context.Background())
	if err != nil || !strings.HasPrefix(commit, strings.TrimSpace(string(sha))) || embedded != strings.TrimSpace(string(sha)) {
		t.Fatalf("verifyControllerCommit() = %q, %q, %v", commit, embedded, err)
	}

	t.Setenv("TEST_IMAGE", "registry/karpenter:e2e-wrong")
	if _, _, err := verifyControllerCommit(context.Background()); err == nil {
		t.Fatal("expected mismatched controller image to fail")
	}
	t.Setenv("TEST_CONTEXT", "other-cluster")
	if _, _, err := verifyControllerCommit(context.Background()); err == nil {
		t.Fatal("expected mismatched cluster context to fail")
	}
	t.Setenv("TEST_CONTEXT", "gke_project_zone_cluster")
	t.Setenv("TEST_IMAGE", "registry/karpenter:e2e-"+strings.TrimSpace(string(sha)))
	t.Setenv("TEST_EMBEDDED", "deadbeef")
	if _, _, err := verifyControllerCommit(context.Background()); err == nil {
		t.Fatal("expected mismatched binary commit to fail")
	}
	t.Setenv("TEST_EMBEDDED", strings.TrimSpace(string(sha)))

	path := filepath.Join(dir, "logs", "controller.log")
	if err := dumpControllerLogs(context.Background(), path, time.Now()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "\"commit\"") {
		t.Fatalf("controller log = %q, %v", data, err)
	}
	t.Setenv("TEST_LOG_FAIL", "1")
	if err := dumpControllerLogs(context.Background(), path, time.Now()); err == nil {
		t.Fatal("expected log retrieval failure")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("failed log dump should be removed: %v", err)
	}
}

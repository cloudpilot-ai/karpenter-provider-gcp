/*
Copyright 2026 The CloudPilot AI Authors.

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
)

func TestControllerAlignmentAcceptsSelectedRelease(t *testing.T) {
	installControllerCommands(t, "v0.7.0", "0123456789abcdef")
	commit, controller, err := verifyControllerCommit(context.Background(), "0.7.0")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSuffix(commit, "-dirty") != testCheckoutCommit(t) || controller != "0123456789abcdef" {
		t.Fatalf("unexpected test/controller metadata: %q, %q", commit, controller)
	}
}

func TestControllerAlignmentTargets(t *testing.T) {
	commit := testCheckoutCommit(t)
	for _, tc := range []struct {
		name, release, imageTag, binaryCommit, wantError string
	}{
		{name: "prefixed release", release: "v0.7.0", imageTag: "v0.7.0", binaryCommit: "0123456789abcdef"},
		{name: "different release", release: "0.7.0", imageTag: "v0.6.1", binaryCommit: "0123456789abcdef", wantError: "does not match requested release"},
		{name: "release must be explicit", imageTag: "v0.7.0", binaryCommit: "0123456789abcdef", wantError: "does not match test commit"},
		{name: "development", imageTag: "e2e-" + commit, binaryCommit: commit},
		{name: "different development image", imageTag: "e2e-other", binaryCommit: commit, wantError: "does not match test commit"},
		{name: "different development binary", imageTag: "e2e-" + commit, binaryCommit: "0123456789abcdef", wantError: "controller binary commit in logs does not match"},
		{name: "missing release binary metadata", release: "0.7.0", imageTag: "v0.7.0", wantError: "controller binary commit not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installControllerCommands(t, tc.imageTag, tc.binaryCommit)
			_, _, err := verifyControllerCommit(context.Background(), tc.release)
			if tc.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("expected %q, got %v", tc.wantError, err)
			}
		})
	}
}

func TestControllerAlignmentAllowsDirtyReleaseTests(t *testing.T) {
	file, err := os.CreateTemp(".", "alignment-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(file.Name()) })
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	installControllerCommands(t, "v0.7.0", "0123456789abcdef")
	commit, _, err := verifyControllerCommit(context.Background(), "0.7.0")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(commit, "-dirty") {
		t.Fatalf("expected dirty test checkout metadata, got %q", commit)
	}
}

func TestControllerAlignmentRejectsDifferentCluster(t *testing.T) {
	installControllerCommands(t, "v0.7.0", "0123456789abcdef")
	t.Setenv("CLUSTER_NAME", "different-cluster")
	if _, _, err := verifyControllerCommit(context.Background(), "0.7.0"); err == nil || !strings.Contains(err.Error(), "cluster context") {
		t.Fatalf("expected cluster mismatch, got %v", err)
	}
}

func testCheckoutCommit(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func installControllerCommands(t *testing.T, imageTag, binaryCommit string) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
case "$*" in
  *'get deployment'*) printf 'public.ecr.aws/cloudpilotai/gcp/karpenter:%s' "$ALIGNMENT_IMAGE_TAG" ;;
  *'logs deployment/'*) printf '{"commit":"%s"}\n' "$ALIGNMENT_BINARY_COMMIT" ;;
  'config current-context') printf 'gke_test-project_test-zone_test-cluster\n' ;;
  *) printf 'unexpected kubectl command: %s\n' "$*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ALIGNMENT_IMAGE_TAG", imageTag)
	t.Setenv("ALIGNMENT_BINARY_COMMIT", binaryCommit)
	t.Setenv("PROJECT_ID", "test-project")
	t.Setenv("CLUSTER_LOCATION", "test-zone")
	t.Setenv("CLUSTER_NAME", "test-cluster")
	t.Setenv("KARPENTER_NAMESPACE", "karpenter-system")
	t.Setenv("KARPENTER_DEPLOYMENT", "karpenter")
}

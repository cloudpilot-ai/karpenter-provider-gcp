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
	"path/filepath"
	"strings"
	"testing"
)

func TestControllerAlignmentAcceptsPublishedRelease(t *testing.T) {
	installAlignmentCommands(t, "v0.7.0", "c3cc2527d28c10b3ba5b2bdb7016c5727793623a", "c3cc252", false)
	commit, controller, err := verifyControllerCommit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if commit != "c3cc2527" || controller != "c3cc252" {
		t.Fatalf("unexpected alignment metadata: %q, %q", commit, controller)
	}
}

func TestControllerAlignmentRejectsMismatchedRelease(t *testing.T) {
	for _, tc := range []struct {
		name          string
		tag           string
		releaseCommit string
		binaryCommit  string
		dirty         bool
		wantError     string
	}{
		{"different commit sharing short prefix", "v0.7.0", "c3cc2527aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "c3cc252", false, "release tag v0.7.0 does not match"},
		{"missing tag", "v0.8.0", "", "c3cc252", false, "resolving release tag"},
		{"different binary", "v0.7.0", "c3cc2527d28c10b3ba5b2bdb7016c5727793623a", "deadbeef", false, "controller binary commit"},
		{"different binary sharing short prefix", "v0.7.0", "c3cc2527d28c10b3ba5b2bdb7016c5727793623a", "c3cc2527aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false, "controller binary commit"},
		{"binary prefix too short", "v0.7.0", "c3cc2527d28c10b3ba5b2bdb7016c5727793623a", "c3", false, "controller binary commit"},
		{"dirty tests", "v0.7.0", "c3cc2527d28c10b3ba5b2bdb7016c5727793623a", "c3cc252", true, "clean test sources"},
		{"dirty binary", "v0.7.0", "c3cc2527d28c10b3ba5b2bdb7016c5727793623a", "c3cc252-dirty", false, "controller binary commit"},
		{"unrecognized image", "latest", "", "c3cc252", false, "deployed controller image"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installAlignmentCommands(t, tc.tag, tc.releaseCommit, tc.binaryCommit, tc.dirty)
			_, _, err := verifyControllerCommit(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("expected %q, got %v", tc.wantError, err)
			}
		})
	}
}

func TestControllerAlignmentRejectsDifferentCluster(t *testing.T) {
	installAlignmentCommands(t, "v0.7.0", "c3cc2527d28c10b3ba5b2bdb7016c5727793623a", "c3cc252", false)
	t.Setenv("CLUSTER_NAME", "different-cluster")
	if _, _, err := verifyControllerCommit(context.Background()); err == nil || !strings.Contains(err.Error(), "cluster context") {
		t.Fatalf("expected cluster mismatch, got %v", err)
	}
}

func TestControllerAlignmentPreservesDevelopmentImages(t *testing.T) {
	for _, dirty := range []bool{false, true} {
		installAlignmentCommands(t, "e2e-c3cc2527", "", "c3cc252", dirty)
		if _, _, err := verifyControllerCommit(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

// Git and kubectl are process boundaries; the fixture supplies their responses.
func installAlignmentCommands(t *testing.T, imageTag, releaseCommit, binaryCommit string, dirty bool) {
	t.Helper()
	dir := t.TempDir()
	commands := map[string]string{
		"git": `#!/bin/sh
case "$*" in
  'rev-parse --short HEAD') printf 'c3cc2527\n' ;;
  'rev-parse HEAD') printf 'c3cc2527d28c10b3ba5b2bdb7016c5727793623a\n' ;;
  'status --porcelain') if [ "$ALIGNMENT_DIRTY" = true ]; then printf ' M changed.go\n'; fi ;;
  'rev-parse --verify refs/tags/v0.7.0^{commit}') printf '%s\n' "$ALIGNMENT_RELEASE_COMMIT" ;;
  *) printf 'unexpected git command: %s\n' "$*" >&2; exit 1 ;;
esac
`,
		"kubectl": `#!/bin/sh
case "$*" in
  *'get deployment'*) printf 'public.ecr.aws/cloudpilotai/gcp/karpenter:%s' "$ALIGNMENT_IMAGE_TAG" ;;
  *'logs deployment/'*) printf '{"commit":"%s"}\n' "$ALIGNMENT_BINARY_COMMIT" ;;
  'config current-context') printf 'gke_test-project_test-zone_test-cluster\n' ;;
  *) printf 'unexpected kubectl command: %s\n' "$*" >&2; exit 1 ;;
esac
`,
	}
	for name, script := range commands {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ALIGNMENT_IMAGE_TAG", imageTag)
	t.Setenv("ALIGNMENT_RELEASE_COMMIT", releaseCommit)
	t.Setenv("ALIGNMENT_BINARY_COMMIT", binaryCommit)
	t.Setenv("ALIGNMENT_DIRTY", "false")
	if dirty {
		t.Setenv("ALIGNMENT_DIRTY", "true")
	}
	t.Setenv("PROJECT_ID", "test-project")
	t.Setenv("CLUSTER_LOCATION", "test-zone")
	t.Setenv("CLUSTER_NAME", "test-cluster")
	t.Setenv("KARPENTER_NAMESPACE", "karpenter-system")
	t.Setenv("KARPENTER_DEPLOYMENT", "karpenter")
}

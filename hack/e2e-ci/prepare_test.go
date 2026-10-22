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

package e2eci

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareRejectsDifferentCheckout(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte("all:\n\ttouch SHOULD_NOT_RUN\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Prepare(context.Background(), dir, filepath.Join(t.TempDir(), "bundle.tar"), frozenFixture(t)); err == nil {
		t.Fatal("non-checkout source accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "SHOULD_NOT_RUN")); !os.IsNotExist(err) {
		t.Fatal("PR Makefile executed")
	}
}

func TestPrepareProducesConsumableBundleWithoutCredentialsOrPRScripts(t *testing.T) {
	f := frozenFixture(t)
	source := filepath.Join(t.TempDir(), "PR checkout with spaces")
	if err := os.MkdirAll(filepath.Join(source, "charts", "karpenter"), 0700); err != nil {
		t.Fatal(err)
	}
	source, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(source, "charts", "karpenter-crd"), 0700)
	os.WriteFile(filepath.Join(source, "charts", "karpenter", "Chart.lock"), []byte("locked\n"), 0600)
	marker := filepath.Join(source, "MAKEFILE_EXECUTED")
	os.WriteFile(filepath.Join(source, "Makefile"), []byte("all:\n\ttouch "+marker+"\n"), 0600)
	fixture := t.TempDir()
	for name, data := range bundleEntries(t, f) {
		if name != ManifestName {
			if err := os.WriteFile(filepath.Join(fixture, name), data, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	tools := t.TempDir()
	writeTool := func(name, script string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(tools, name), []byte("#!/bin/sh\nset -eu\n"+script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	// These are process-boundary fakes for git/Go/ko/Helm, not internal collaborators.
	writeTool("git", `case "$1" in
 rev-parse) printf '%s\n' '`+f.HeadSHA+`';;
 status|diff) :;;
 *) exit 44;;
 esac
 `)
	preflight := `test "$(pwd)" = '` + source + `'
  test "$GOWORK" = off
 test -z "${GH_TOKEN:-}${GOOGLE_APPLICATION_CREDENTIALS:-}${ACTIONS_ID_TOKEN_REQUEST_TOKEN:-}${KUBECONFIG:-}"
 `
	writeTool("go", preflight+`test "$GOOS/$GOARCH" = linux/amd64
 case "$1" in
 list) printf '{"Path":"github.com/onsi/ginkgo/v2","Version":"v2.33.0"}';;
 test|build) while [ "$1" != -o ]; do shift; done; shift; name="${1##*/}"; cp '`+fixture+`/'"$name" "$1";;
 *) exit 45;;
 esac
 `)
	writeTool("ko", preflight+`test -z "${GOOS:-}${GOARCH:-}"
 case " $* " in *' --push=false '*) :;; *) exit 50;; esac
 case " $* " in *' --platform=linux/amd64 '*) :;; *) exit 51;; esac
 case " $* " in *' --sbom=none '*) :;; *) exit 52;; esac
 case " $* " in *' --image-label=org.opencontainers.image.revision=`+f.HeadSHA+` '*) :;; *) exit 53;; esac
 test "$KO_CONFIG_PATH" != .ko.yaml
 for arg in "$@"; do case "$arg" in --oci-layout-path=*) layout="${arg#*=}";; esac; done
 mkdir "$layout"
 tar -xf '`+filepath.Join(fixture, ControllerArchive)+`' -C "$layout"
 `)
	writeTool("helm", preflight+`case "$1" in
 dependency) test "$2" = build;;
 package) test "$3" = --destination; name="${2##*/}"; cp '`+fixture+`/'"$name"'.tgz' "$4/$name-0.0.0.tgz";;
 *) exit 46;;
 esac
 `)
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GOOS", "darwin")
	t.Setenv("GOARCH", "arm64")
	t.Setenv("GH_TOKEN", "must-not-inherit")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "must-not-read")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "must-not-inherit")
	t.Setenv("KUBECONFIG", "must-not-read")
	os.WriteFile(filepath.Join(source, "charts", "karpenter", "Chart.yaml"), []byte("apiVersion: v2\nname: karpenter\n"), 0600)
	crdMetadata := filepath.Join(source, "charts", "karpenter-crd", "Chart.yaml")
	os.WriteFile(crdMetadata, []byte("apiVersion: v2\nname: karpenter-crd\ndependencies:\n  - name: unpinned\n    version: 1.0.0\n    repository: https://example.invalid\n"), 0600)
	if err := Prepare(context.Background(), source, filepath.Join(t.TempDir(), "reject.tar"), f); err == nil || !strings.Contains(err.Error(), "locked dependencies") {
		t.Fatalf("unlocked CRD dependency not rejected: %v", err)
	}
	os.WriteFile(crdMetadata, []byte("apiVersion: v2\nname: karpenter-crd\n"), 0600)
	output := filepath.Join(t.TempDir(), "bundle.tar")
	if err := Prepare(context.Background(), source, output, f); err != nil {
		t.Fatal(err)
	}
	archive, err := os.Open(output)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	b, err := VerifyBundle(archive, t.TempDir(), f)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(b.Root)
	if b.Manifest.Source != f {
		t.Fatal("lost source tuple")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("PR Makefile executed")
	}
	if err := Prepare(context.Background(), source, filepath.Join(source, "bundle.tar"), f); err == nil {
		t.Fatal("output in PR checkout accepted")
	}
	generated := filepath.Join(source, "untracked_test.go")
	goTool, err := os.ReadFile(filepath.Join(tools, "go"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(tools, "go"), append(goTool, []byte("\ntouch '"+generated+"'\n")...), 0700)
	writeTool("git", `case "$1" in
 rev-parse) printf '%s\n' '`+f.HeadSHA+`';;
 status) if [ -f '`+generated+`' ]; then printf '?? untracked_test.go\n'; fi;;
 diff) :;;
 *) exit 44;;
 esac
 `)
	if err := Prepare(context.Background(), source, filepath.Join(t.TempDir(), "changed.tar"), f); err == nil || !strings.Contains(err.Error(), "source changed") {
		t.Fatalf("generated source accepted: %v", err)
	}

}

func TestFetchSourceRejectsMovedUpstreamPRRef(t *testing.T) {
	f := frozenFixture(t)
	tools := t.TempDir()
	log := filepath.Join(t.TempDir(), "git-argv")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> '` + log + `'
case "$1" in
 rev-parse) printf '%s\n' '` + toolSHA + `';;
 *) :;;
esac
`
	if err := os.WriteFile(filepath.Join(tools, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	destination := filepath.Join(t.TempDir(), "source")
	err := FetchSource(context.Background(), destination, f)
	if err == nil || !strings.Contains(err.Error(), "frozen SHA") {
		t.Fatalf("moved PR ref not rejected: %v", err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatal("failed fetch left a usable checkout")
	}
	argv, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(argv), "https://github.com/"+Repository+".git refs/pull/12/head") || strings.Contains(string(argv), "checkout") || strings.Contains(string(argv), "contributor/fork") {
		t.Fatalf("unsafe frozen source acquisition: %s", argv)
	}
}

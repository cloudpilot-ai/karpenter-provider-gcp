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
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/onsi/ginkgo/v2/reporters"
	"github.com/onsi/ginkgo/v2/types"

	e2eci "github.com/cloudpilot-ai/karpenter-provider-gcp/hack/e2e-ci"
)

func TestPreparedSelection(t *testing.T) {
	for _, preset := range []string{"standard", "gpu", "full"} {
		filter, resolved, err := preparedSelection(preset)
		if err != nil {
			t.Fatal(err)
		}
		local := preset
		if preset == "full" {
			local = "all"
		}
		want, err := selectFilter("does-not-exist", local)
		if err != nil || filter != want || resolved != local {
			t.Fatalf("%s: %q %q %v", preset, filter, resolved, err)
		}
		match, err := types.ParseLabelFilter(filter)
		if err != nil {
			t.Fatal(err)
		}
		if preset == "full" && (!match([]string{"suite:gpu"}) || !match([]string{"suite:local-ssd-extended"})) {
			t.Fatal("full excluded expensive features")
		}
	}
	if _, _, err := preparedSelection("all"); err == nil {
		t.Fatal("CI accepted local-only preset")
	}
}

func TestNativeReportsRejectZeroExecutedTests(t *testing.T) {
	dir := t.TempDir()
	report := types.Report{SuiteSucceeded: true, SpecReports: types.SpecReports{{LeafNodeType: types.NodeTypeIt, State: types.SpecStateSkipped}}}
	writeNativeFixture(t, dir, report)
	if _, _, err := readNativeReports(dir); err == nil {
		t.Fatal("all-skipped report accepted as executed tests")
	}
}

func writeNativeFixture(t *testing.T, dir string, report types.Report) {
	t.Helper()
	if err := reporters.GenerateJSONReport(report, filepath.Join(dir, "results.json")); err != nil {
		t.Fatal(err)
	}
	if err := reporters.GenerateJUnitReport(report, filepath.Join(dir, "junit.xml")); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedControllerRequiresExactSHAAndDigest(t *testing.T) {
	sha := strings.Repeat("a", 40)
	digest := "sha256:" + strings.Repeat("b", 64)
	image := "example.invalid/controller@" + digest
	for _, tc := range []struct {
		name, commit, actualDigest string
		wantErr                    bool
	}{
		{"exact", sha, digest, false},
		{"short-sha", sha[:7], digest, true},
		{"wrong-sha", strings.Repeat("c", 40), digest, true},
		{"dirty-sha", sha + "-dirty", digest, true},
		{"wrong-running-image", sha, "sha256:" + strings.Repeat("c", 64), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeController(t, image, tc.actualDigest, tc.commit)
			err := verifyPreparedController(context.Background(), image, sha)
			if (err != nil) != tc.wantErr {
				t.Fatalf("got %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

func fakeController(t *testing.T, image, digest, commit string) {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"deployment": fmt.Sprintf(`{"spec":{"replicas":1,"selector":{"matchLabels":{"app":"karpenter"}},"template":{"spec":{"containers":[{"name":"karpenter","image":%q}]}}}}`, image),
		"pods":       fmt.Sprintf(`{"items":[{"metadata":{"name":"controller-1"},"spec":{"containers":[{"name":"karpenter","image":%q}]},"status":{"containerStatuses":[{"name":"karpenter","ready":true,"imageID":%q}]}}]}`, image, "docker-pullable://example.invalid/controller@"+digest),
		"logs":       fmt.Sprintf("{\"commit\":%q}\n", commit),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	script := `#!/bin/sh
case "$*" in
 *"rollout status"*) exit 0 ;;
 *"get deployment"*) cat "$CONTROLLER_FIXTURE/deployment" ;;
 *"get pods"*) cat "$CONTROLLER_FIXTURE/pods" ;;
 *"logs"*) cat "$CONTROLLER_FIXTURE/logs" ;;
 *) exit 97 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTROLLER_FIXTURE", dir)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestPreparedExecutionPersistsNativeReports(t *testing.T) {
	bundle, source, image := preparedFixture(t)
	fakeController(t, image, strings.Split(image, "@")[1], source.HeadSHA)
	t.Setenv("PROJECT_ID", "fixture-pass")
	t.Setenv("GITHUB_TOKEN", "must-not-reach-tests")
	output := filepath.Join(t.TempDir(), "run")
	outcome, err := executePrepared(context.Background(), preparedExecution{Bundle: bundle, Source: source, Image: image, Workers: 20, OutputDir: output})
	if err != nil {
		t.Fatal(err)
	}
	rendered, readErr := os.ReadFile(filepath.Join(output, "report.md"))
	if readErr != nil || !strings.Contains(html.UnescapeString(string(rendered)), strings.Split(image, "@")[1]) {
		t.Fatalf("report omitted exact image digest: %s %v", rendered, readErr)
	}
	if outcome.Tests != statePassed || outcome.Reports != statePassed || outcome.Counts.Executed != 1 || outcome.Cleanup != stateNotRun || outcome.Overall() != stateIncomplete {
		t.Fatalf("%+v", outcome)
	}
	for _, name := range []string{"results.json", "junit.xml", "outcome.json", "report.md", "suite.log"} {
		info, err := os.Stat(filepath.Join(output, name))
		if err != nil || info.Size() == 0 {
			t.Fatalf("%s not durable: %v", name, err)
		}
	}
}

var nativeOnce sync.Once
var nativeCLI, nativeSuite []byte
var nativeErr error

func nativeExecutables(t *testing.T) ([]byte, []byte) {
	t.Helper()
	nativeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "native-fixture-")
		if err != nil {
			nativeErr = err
			return
		}
		defer os.RemoveAll(dir)
		cli, suite := filepath.Join(dir, "ginkgo"), filepath.Join(dir, "e2e.test")
		for _, args := range [][]string{{"build", "-o", cli, "github.com/onsi/ginkgo/v2/ginkgo"}, {"test", "-c", "-o", suite, "./testdata/native"}} {
			cmd := exec.Command("go", args...)
			if output, err := cmd.CombinedOutput(); err != nil {
				nativeErr = fmt.Errorf("%v: %w: %s", args, err, output)
				return
			}
		}
		nativeCLI, nativeErr = os.ReadFile(cli)
		if nativeErr != nil {
			return
		}
		nativeSuite, nativeErr = os.ReadFile(suite)
	})
	if nativeErr != nil {
		t.Fatal(nativeErr)
	}
	return nativeCLI, nativeSuite
}

func preparedFixture(t *testing.T) (*e2eci.Bundle, e2eci.Frozen, string) {
	t.Helper()
	source := e2eci.Frozen{Schema: 1, BaseRepositoryID: e2eci.RepositoryID, PRNumber: 7, HeadRepositoryID: 3, HeadRepository: "contributor/fork", HeadSHA: strings.Repeat("a", 40), Dispatch: e2eci.Dispatch{
		Repository: e2eci.Repository, Ref: "refs/heads/main", WorkflowRef: e2eci.Repository + "/" + e2eci.WorkflowPath + "@refs/heads/main", WorkflowSHA: strings.Repeat("b", 40), Event: "workflow_dispatch", Actor: "maintainer", ActorID: 1, RunID: 9, RunAttempt: 1, PR: "7", Preset: "standard",
	}}
	cli, suite := nativeExecutables(t)
	oci, digest := controllerArchive(t, source.HeadSHA)
	files := map[string][]byte{e2eci.TestBinary: suite, e2eci.GinkgoBinary: cli, e2eci.ControllerArchive: oci, e2eci.ControllerChart: []byte("previously validated chart"), e2eci.CRDChart: []byte("previously validated CRDs")}
	b := &e2eci.Bundle{Root: t.TempDir(), Manifest: e2eci.Manifest{Schema: 1, Source: source, Platform: "linux/amd64", GinkgoVersion: "v2.33.0", Outcome: "success"}}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(b.Root, name), data, 0700); err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256(data)
		b.Manifest.Files = append(b.Manifest.Files, e2eci.FileDigest{Name: name, Size: int64(len(data)), SHA256: hex.EncodeToString(h[:])})
	}
	return b, source, "example.invalid/controller@" + digest
}

func controllerArchive(t *testing.T, sha string) ([]byte, string) {
	t.Helper()
	files := map[string][]byte{"oci-layout": []byte(`{"imageLayoutVersion":"1.0.0"}`)}
	blob := func(data []byte, media string) map[string]any {
		hash := sha256.Sum256(data)
		digest := hex.EncodeToString(hash[:])
		files["blobs/sha256/"+digest] = data
		return map[string]any{"mediaType": media, "digest": "sha256:" + digest, "size": len(data)}
	}
	marshal := func(value any) []byte {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	config := blob([]byte(`{"architecture":"amd64","os":"linux","config":{"Labels":{"org.opencontainers.image.revision":"`+sha+`"}}}`), "application/vnd.oci.image.config.v1+json")
	layer := blob([]byte("opaque image layer"), "application/vnd.oci.image.layer.v1.tar+gzip")
	image := blob(marshal(map[string]any{"schemaVersion": 2, "config": config, "layers": []any{layer}}), "application/vnd.oci.image.manifest.v1+json")
	files["index.json"] = marshal(map[string]any{"schemaVersion": 2, "manifests": []any{image}})
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(data)), Typeflag: tar.TypeReg, Mode: 0600}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), image["digest"].(string)
}

func TestNativeReportsRejectTrailingJUnitData(t *testing.T) {
	dir := t.TempDir()
	writeNativeFixture(t, dir, types.Report{SuiteSucceeded: true, SpecReports: types.SpecReports{{LeafNodeType: types.NodeTypeIt, State: types.SpecStatePassed, LeafNodeText: "passed"}}})
	f, err := os.OpenFile(filepath.Join(dir, "junit.xml"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("<unexpected/>trailing"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readNativeReports(dir); err == nil {
		t.Fatal("trailing JUnit corruption accepted")
	}
}

func TestPreparedExecutionFailureAndSelections(t *testing.T) {
	for _, tc := range []struct {
		preset, project string
		executed        int
		want            outcomeState
	}{
		{"standard", "fixture-fail", 1, stateFailed},
		{"gpu", "fixture-pass", 1, statePassed},
		{"full", "fixture-pass", 3, statePassed},
	} {
		t.Run(tc.preset, func(t *testing.T) {
			bundle, source, image := preparedFixture(t)
			source.Dispatch.Preset = tc.preset
			bundle.Manifest.Source = source
			fakeController(t, image, strings.Split(image, "@")[1], source.HeadSHA)
			t.Setenv("PROJECT_ID", tc.project)
			dir := filepath.Join(t.TempDir(), "run")
			outcome, err := executePrepared(context.Background(), preparedExecution{Bundle: bundle, Source: source, Image: image, Workers: 20, OutputDir: dir})
			if (err != nil) != (tc.want != statePassed) || outcome.Tests != tc.want || outcome.Counts.Executed != tc.executed || outcome.Reports != statePassed {
				t.Fatalf("%+v %v", outcome, err)
			}
			if _, err := os.Stat(filepath.Join(dir, "report.md")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPreparedExecutionCancellationKillsChildrenAndPersists(t *testing.T) {
	bundle, source, image := preparedFixture(t)
	fakeController(t, image, strings.Split(image, "@")[1], source.HeadSHA)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { cancel(); w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	t.Setenv("PROJECT_ID", server.URL)
	dir := filepath.Join(t.TempDir(), "run")
	outcome, err := executePrepared(ctx, preparedExecution{Bundle: bundle, Source: source, Image: image, Workers: 20, OutputDir: dir})
	if err == nil || outcome.Tests != stateCancelled || outcome.Overall() != stateCancelled {
		t.Fatalf("%+v %v", outcome, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "outcome.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"tests": "cancelled"`) {
		t.Fatalf("cancellation not durable: %s", data)
	}
	if _, err := os.Stat(filepath.Join(dir, "report.md")); err != nil {
		t.Fatal(err)
	}
}

func replacePreparedFile(t *testing.T, b *e2eci.Bundle, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(b.Root, name), data, 0700); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	for i := range b.Manifest.Files {
		if b.Manifest.Files[i].Name == name {
			b.Manifest.Files[i].Size = int64(len(data))
			b.Manifest.Files[i].SHA256 = hex.EncodeToString(hash[:])
		}
	}
}

func TestPreparedExecutionNeverAcceptsAbsentCorruptOrZeroReports(t *testing.T) {
	for _, mode := range []string{"missing", "corrupt", "zero", "early-failure"} {
		t.Run(mode, func(t *testing.T) {
			bundle, source, image := preparedFixture(t)
			fakeController(t, image, strings.Split(image, "@")[1], source.HeadSHA)
			fixture := t.TempDir()
			writeNativeFixture(t, fixture, types.Report{SuiteSucceeded: true, SpecReports: types.SpecReports{{LeafNodeType: types.NodeTypeIt, State: types.SpecStateSkipped}}})
			script := "#!/bin/sh\nfor arg in \"$@\";do case \"$arg\" in --output-dir=*) dir=${arg#*=};;esac;done\n"
			switch mode {
			case "corrupt":
				script += "printf '{broken' > \"$dir/results.json\"\n"
			case "zero":
				script += fmt.Sprintf("cp %q/results.json \"$dir/results.json\"\ncp %q/junit.xml \"$dir/junit.xml\"\n", fixture, fixture)
			case "early-failure":
				script += "exit 7\n"
			}
			replacePreparedFile(t, bundle, e2eci.GinkgoBinary, []byte(script))
			dir := filepath.Join(t.TempDir(), "run")
			outcome, err := executePrepared(context.Background(), preparedExecution{Bundle: bundle, Source: source, Image: image, Workers: 20, OutputDir: dir})
			if err == nil || outcome.Tests == statePassed || outcome.Reports == statePassed || outcome.Overall() == statePassed {
				t.Fatalf("%+v %v", outcome, err)
			}
			for _, name := range []string{"outcome.json", "report.md"} {
				if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestPreparedExecutionRejectsChangedInputsBeforeControllerOrTests(t *testing.T) {
	for _, mode := range []string{"source", "digest", "workers", "test-binary", "chart", "symlink", "duplicate", "platform"} {
		t.Run(mode, func(t *testing.T) {
			bundle, source, image := preparedFixture(t)
			workers := 20
			switch mode {
			case "source":
				source.HeadSHA = strings.Repeat("d", 40)
			case "digest":
				image = "example.invalid/controller@sha256:" + strings.Repeat("d", 64)
			case "workers":
				workers = 19
			case "test-binary":
				if err := os.WriteFile(filepath.Join(bundle.Root, e2eci.TestBinary), []byte("changed"), 0700); err != nil {
					t.Fatal(err)
				}
			case "chart":
				if err := os.WriteFile(filepath.Join(bundle.Root, e2eci.ControllerChart), []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				path := filepath.Join(bundle.Root, e2eci.TestBinary)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/not/a/binary", path); err != nil {
					t.Fatal(err)
				}
			case "duplicate":
				bundle.Manifest.Files[1] = bundle.Manifest.Files[0]
			case "platform":
				bundle.Manifest.Platform = "darwin/arm64"
			}
			t.Setenv("PATH", t.TempDir())
			dir := filepath.Join(t.TempDir(), "run")
			outcome, err := executePrepared(context.Background(), preparedExecution{Bundle: bundle, Source: source, Image: image, Workers: workers, OutputDir: dir})
			if err == nil || outcome.Infrastructure != stateFailed || outcome.Tests != stateNotRun {
				t.Fatalf("%+v %v", outcome, err)
			}
			if _, err := os.Stat(filepath.Join(dir, "suite.log")); !os.IsNotExist(err) {
				t.Fatalf("test process may have launched: %v", err)
			}
		})
	}
}

func TestPreparedExecutionRejectsReusedOutput(t *testing.T) {
	bundle, source, image := preparedFixture(t)
	dir := t.TempDir()
	writeNativeFixture(t, dir, types.Report{SuiteSucceeded: true, SpecReports: types.SpecReports{{LeafNodeType: types.NodeTypeIt, State: types.SpecStatePassed}}})
	if _, err := executePrepared(context.Background(), preparedExecution{Bundle: bundle, Source: source, Image: image, Workers: 20, OutputDir: dir}); err == nil {
		t.Fatal("reused output accepted stale native success")
	}
}

func TestNativeReportValidation(t *testing.T) {
	for _, mode := range []string{"missing-json", "missing-junit", "corrupt-json", "corrupt-junit", "disagree", "oversized", "symlink", "empty-array"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			report := types.Report{SuiteSucceeded: true, SpecReports: types.SpecReports{{LeafNodeType: types.NodeTypeIt, State: types.SpecStatePassed, LeafNodeText: "native passed"}}}
			writeNativeFixture(t, dir, report)
			jsonPath, junitPath := filepath.Join(dir, "results.json"), filepath.Join(dir, "junit.xml")
			switch mode {
			case "missing-json":
				if err := os.Remove(jsonPath); err != nil {
					t.Fatal(err)
				}
			case "missing-junit":
				if err := os.Remove(junitPath); err != nil {
					t.Fatal(err)
				}
			case "corrupt-json":
				if err := os.WriteFile(jsonPath, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "corrupt-junit":
				if err := os.WriteFile(junitPath, []byte("<invalid>"), 0600); err != nil {
					t.Fatal(err)
				}
			case "disagree":
				report.SpecReports[0].State = types.SpecStateFailed
				if err := reporters.GenerateJUnitReport(report, junitPath); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				if err := os.Truncate(jsonPath, maxNativeBytes+1); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(jsonPath, jsonPath+"-target"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(jsonPath+"-target", jsonPath); err != nil {
					t.Fatal(err)
				}
			case "empty-array":
				if err := os.WriteFile(jsonPath, []byte("[]"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := readNativeReports(dir); err == nil {
				t.Fatal("invalid native evidence accepted")
			}
		})
	}
}

func TestPreparedControllerRejectsUnreadyOrMismatchedDesiredState(t *testing.T) {
	sha, digest := strings.Repeat("a", 40), "sha256:"+strings.Repeat("b", 64)
	image := "example.invalid/controller@" + digest
	for _, mode := range []string{"tagged-deployment", "wrong-pod-spec", "unready", "no-pods", "missing-commit", "replica-mismatch"} {
		t.Run(mode, func(t *testing.T) {
			fakeController(t, image, digest, sha)
			dir := os.Getenv("CONTROLLER_FIXTURE")
			file := filepath.Join(dir, "pods")
			if mode == "tagged-deployment" || mode == "replica-mismatch" {
				file = filepath.Join(dir, "deployment")
			}
			if mode == "missing-commit" {
				file = filepath.Join(dir, "logs")
			}
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "tagged-deployment", "wrong-pod-spec":
				data = []byte(strings.Replace(string(data), image, "example.invalid/controller:e2e-aaaaaaa", 1))
			case "unready":
				data = []byte(strings.Replace(string(data), `"ready":true`, `"ready":false`, 1))
			case "no-pods":
				data = []byte(`{"items":[]}`)
			case "missing-commit":
				data = []byte(`{"message":"no version"}`)
			case "replica-mismatch":
				data = []byte(strings.Replace(string(data), `"replicas":1`, `"replicas":2`, 1))
			}
			if err := os.WriteFile(file, data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := verifyPreparedController(context.Background(), image, sha); err == nil {
				t.Fatal("misaligned controller accepted")
			}
		})
	}
}

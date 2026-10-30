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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/onsi/ginkgo/v2/reporters"
	"github.com/onsi/ginkgo/v2/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func TestJSONReportsRejectZeroExecutedTests(t *testing.T) {
	dir := t.TempDir()
	report := types.Report{SuiteSucceeded: true, SpecReports: types.SpecReports{{LeafNodeType: types.NodeTypeIt, State: types.SpecStateSkipped}}}
	writeNativeFixture(t, dir, report)
	if _, _, err := readJSONReports(filepath.Join(dir, "results.json")); err == nil {
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
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(strings.ReplaceAll(script, "$CONTROLLER_FIXTURE", dir)), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTROLLER_FIXTURE", dir)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

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

func TestJSONReportValidation(t *testing.T) {
	for _, mode := range []string{"missing-json", "corrupt-json", "oversized", "symlink", "empty-array", "incomplete-state", "suite-limit", "spec-limit"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			report := types.Report{SuiteSucceeded: true, SpecReports: types.SpecReports{{LeafNodeType: types.NodeTypeIt, State: types.SpecStatePassed, LeafNodeText: "native passed"}}}
			writeNativeFixture(t, dir, report)
			jsonPath := filepath.Join(dir, "results.json")
			switch mode {
			case "missing-json":
				if err := os.Remove(jsonPath); err != nil {
					t.Fatal(err)
				}
			case "corrupt-json":
				if err := os.WriteFile(jsonPath, []byte("{"), 0600); err != nil {
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
			case "incomplete-state":
				report.SpecReports[0].State = types.SpecStateInvalid
				if err := reporters.GenerateJSONReport(report, jsonPath); err != nil {
					t.Fatal(err)
				}
			case "suite-limit":
				data, err := json.Marshal(make([]types.Report, 65))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(jsonPath, data, 0600); err != nil {
					t.Fatal(err)
				}
			case "spec-limit":
				report.SpecReports = make(types.SpecReports, maxNativeSpecs+1)
				if err := reporters.GenerateJSONReport(report, jsonPath); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := readJSONReports(filepath.Join(dir, "results.json")); err == nil {
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

var nativeOnce sync.Once
var nativeCLI, nativeSuite []byte
var nativeErr error

func preparedFixture(t *testing.T) (preparedExecution, string) {
	t.Helper()
	cli, suite := nativeExecutables(t)
	root := t.TempDir()
	input := preparedExecution{SourceSHA: strings.Repeat("a", 40), Selection: "standard", ControllerArchive: filepath.Join(root, "controller-image.tar"), ControllerChart: filepath.Join(root, "controller-chart.tgz"), CRDChart: filepath.Join(root, "crd-chart.tgz"), TestBinary: filepath.Join(root, "e2e.test"), GinkgoBinary: filepath.Join(root, "ginkgo"), OutputDir: filepath.Join(t.TempDir(), "results")}
	for path, data := range map[string][]byte{input.ControllerArchive: []byte("preauth-validated raw archive"), input.ControllerChart: []byte("previously validated chart"), input.CRDChart: []byte("previously validated CRDs"), input.TestBinary: suite, input.GinkgoBinary: cli} {
		if err := os.WriteFile(path, data, 0700); err != nil {
			t.Fatal(err)
		}
	}
	return input, "example.invalid/controller@sha256:" + strings.Repeat("b", 64)
}

func fixtureCheckout(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

type runtimeVerdict struct {
	SourceSHA string `json:"source_sha"`
	TestExit  *int   `json:"test_exit"`
	CleanupOK bool   `json:"cleanup_ok"`
}

func TestPreparedExecutionNativeSelectionsAndFailure(t *testing.T) {
	for _, tc := range []struct {
		selection, project string
		executed           int
		fails              bool
	}{
		{"standard", "fixture-pass", 1, false}, {"gpu", "fixture-pass", 1, false}, {"full", "fixture-pass", 3, false}, {"standard", "fixture-fail", 1, true},
	} {
		t.Run(tc.selection+tc.project, func(t *testing.T) {
			input, _ := preparedFixture(t)
			input.Selection = tc.selection
			if err := os.Mkdir(input.OutputDir, 0700); err != nil {
				t.Fatal(err)
			}
			exit, err := executePrepared(context.Background(), input, []string{"PROJECT_ID=" + tc.project})
			if (err != nil) != tc.fails || exit == nil || (*exit != 0) != tc.fails {
				t.Fatalf("exit=%v err=%v", exit, err)
			}
			_, counts, err := readJSONReports(filepath.Join(input.OutputDir, "results.json"))
			if err != nil || counts.Executed != tc.executed {
				t.Fatalf("counts=%+v err=%v", counts, err)
			}
			junit, err := os.ReadFile(filepath.Join(input.OutputDir, "junit.xml"))
			if err != nil || len(junit) == 0 {
				t.Fatalf("missing generated private JUnit: %v", err)
			}
			data, err := os.ReadFile(filepath.Join(input.OutputDir, "suite.log"))
			if err != nil || len(data) == 0 {
				t.Fatalf("missing private suite log: %s %v", data, err)
			}
		})
	}
}

func TestPreparedExecutionCannotPassWithoutNativeEvidence(t *testing.T) {
	for _, mode := range []string{"missing", "corrupt", "zero", "failed-suite", "early-failure"} {
		t.Run(mode, func(t *testing.T) {
			input, _ := preparedFixture(t)
			if err := os.Mkdir(input.OutputDir, 0700); err != nil {
				t.Fatal(err)
			}
			script := "#!/bin/sh\n"
			switch mode {
			case "corrupt":
				script += "echo '{broken' > results.json\n"
			case "zero":
				writeNativeFixture(t, input.OutputDir, types.Report{SuiteSucceeded: true, SpecReports: types.SpecReports{{LeafNodeType: types.NodeTypeIt, State: types.SpecStateSkipped}}})
			case "failed-suite":
				writeNativeFixture(t, input.OutputDir, types.Report{SuiteSucceeded: false, SpecReports: types.SpecReports{{LeafNodeType: types.NodeTypeIt, State: types.SpecStatePassed}}})
			case "early-failure":
				script += "exit 7\n"
			}
			if err := os.WriteFile(input.GinkgoBinary, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			exit, err := executePrepared(context.Background(), input, []string{})
			if err == nil || exit == nil {
				t.Fatalf("missing failure: %v %v", exit, err)
			}
		})
	}
}

func TestPreparedCommandNeutralFlagsAndFinalVerdict(t *testing.T) {
	target, input, client, _ := lifecycleFixture(t)
	setLifecycleTargetEnv(t, target)
	serveLeaseBoundary(t, client)
	t.Setenv("GITHUB_TOKEN", "must-not-enter-runtime")
	command := newRunnerCommand()
	command.SetArgs([]string{"prepared", "--source-sha", input.SourceSHA, "--selection", input.Selection, "--controller-archive", input.ControllerArchive, "--controller-chart", input.ControllerChart, "--crd-chart", input.CRDChart, "--test-binary", input.TestBinary, "--ginkgo-binary", input.GinkgoBinary, "--results-dir", input.OutputDir, "--lock-id", "neutral-cli"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	verdict := readRuntimeVerdict(t, input.OutputDir)
	if verdict.SourceSHA != input.SourceSHA || verdict.TestExit == nil || *verdict.TestExit != 0 || !verdict.CleanupOK {
		t.Fatalf("%+v", verdict)
	}
}

func TestPreparedCommandRejectsReusedOutputAndInvalidFilesBeforeCloud(t *testing.T) {
	for _, mode := range []string{"reused-output", "bad-sha", "bad-selection", "symlink-file", "missing-file", "artifact-results-overlap"} {
		t.Run(mode, func(t *testing.T) {
			input, _ := preparedFixture(t)
			switch mode {
			case "reused-output":
				if err := os.Mkdir(input.OutputDir, 0700); err != nil {
					t.Fatal(err)
				}
				writeNativeFixture(t, input.OutputDir, types.Report{SuiteSucceeded: true, SpecReports: types.SpecReports{{LeafNodeType: types.NodeTypeIt, State: types.SpecStatePassed}}})
			case "bad-sha":
				input.SourceSHA = "aaaaaaa"
			case "bad-selection":
				input.Selection = "all"
			case "symlink-file":
				original := input.TestBinary
				input.TestBinary += "-link"
				if err := os.Symlink(original, input.TestBinary); err != nil {
					t.Fatal(err)
				}
			case "missing-file":
				input.CRDChart += "-missing"
			case "artifact-results-overlap":
				input.OutputDir = filepath.Join(filepath.Dir(input.TestBinary), "results")
			}
			t.Setenv("PATH", t.TempDir())
			client := &memoryLeaseClient{}
			if err := runPreparedLifecycle(context.Background(), client, "ci", lifecycleTarget{}, input); err == nil {
				t.Fatal("unsafe input accepted")
			}
			if client.lease != nil {
				t.Fatal("attempted acquisition before input validation")
			}
		})
	}
}

func TestPreparedCredentialsAndArtifactRootsAreIsolated(t *testing.T) {
	for _, mode := range []string{"adc-in-artifacts", "kubeconfig-in-results", "config-contains-artifacts", "symlink-alias", "temp-in-results"} {
		t.Run(mode, func(t *testing.T) {
			input, _ := preparedFixture(t)
			switch mode {
			case "adc-in-artifacts":
				t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(filepath.Dir(input.TestBinary), "adc.json"))
			case "kubeconfig-in-results":
				t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "safe")+string(os.PathListSeparator)+filepath.Join(input.OutputDir, "kubeconfig"))
			case "config-contains-artifacts":
				t.Setenv("CLOUDSDK_CONFIG", filepath.Dir(input.TestBinary))
			case "symlink-alias":
				alias := filepath.Join(t.TempDir(), "artifacts")
				if err := os.Symlink(filepath.Dir(input.TestBinary), alias); err != nil {
					t.Fatal(err)
				}
				t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(alias, "adc.json"))
			case "temp-in-results":
				t.Setenv("TMPDIR", input.OutputDir)
			}
			if err := validatePreparedExecution(&input); err == nil {
				t.Fatal("overlapping credentials/artifact/results paths accepted")
			}
		})
	}
}

func TestValidateTargetRemainsCloudFree(t *testing.T) {
	target, _, _, _ := lifecycleFixture(t)
	setLifecycleTargetEnv(t, target)
	t.Setenv("PATH", t.TempDir())
	command := newRunnerCommand()
	command.SetArgs([]string{"validate-target"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("E2E_REGION", "")
	command = newRunnerCommand()
	command.SetArgs([]string{"validate-target"})
	if err := command.Execute(); err == nil {
		t.Fatal("preauth validation derived missing region")
	}
}

func TestPreparedShellFailureDoesNotLaunchTests(t *testing.T) {
	for _, mode := range []string{"helm-failure", "wrong-digest", "malformed-reference"} {
		t.Run(mode, func(t *testing.T) {
			target, input, client, dir := lifecycleFixture(t)
			switch mode {
			case "helm-failure":
				if err := os.WriteFile(filepath.Join(dir, "fail-helm"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong-digest":
				if err := os.WriteFile(filepath.Join(dir, "digest"), []byte("sha256:"+strings.Repeat("0", 64)), 0600); err != nil {
					t.Fatal(err)
				}
			case "malformed-reference":
				if err := os.WriteFile(filepath.Join(dir, "digest"), []byte("sha256:"+strings.Repeat("b", 64)+"\ninjected"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := runPreparedLifecycle(context.Background(), client, "ci", target, input); err == nil {
				t.Fatal("failed deployment/identity passed")
			}
			verdict := readRuntimeVerdict(t, input.OutputDir)
			if verdict.TestExit != nil || !verdict.CleanupOK {
				t.Fatalf("%+v", verdict)
			}
			if _, err := os.Stat(filepath.Join(input.OutputDir, "suite.log")); !os.IsNotExist(err) {
				t.Fatal("tests launched after deployment/identity failure")
			}
		})
	}
}

func TestPreparedMaintenanceBlockCannotPass(t *testing.T) {
	target, input, client, dir := lifecycleFixture(t)
	client.lease.Annotations = map[string]string{maintenanceKey: "incomplete-drain"}
	if err := runPreparedLifecycle(context.Background(), client, "ci", target, input); !errors.Is(err, errTargetMaintenance) {
		t.Fatalf("%v", err)
	}
	verdict := readRuntimeVerdict(t, input.OutputDir)
	if verdict.TestExit != nil || verdict.CleanupOK {
		t.Fatalf("%+v", verdict)
	}
	operations, _ := os.ReadFile(filepath.Join(dir, "operations"))
	for _, mutation := range []string{"push ", "upgrade ", "delete "} {
		if strings.Contains(string(operations), mutation) {
			t.Fatalf("blocked target mutated: %s", operations)
		}
	}
}

func TestPreparedTestFailureRemainsPrimaryWhenCleanupFails(t *testing.T) {
	target, input, client, dir := lifecycleFixture(t)
	if err := os.WriteFile(input.GinkgoBinary, []byte("#!/bin/sh\nexit 7\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "incomplete"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	err := runPreparedLifecycle(context.Background(), client, "ci", target, input)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("primary test exit lost: %v", err)
	}
	verdict := readRuntimeVerdict(t, input.OutputDir)
	if verdict.TestExit == nil || *verdict.TestExit != 7 || verdict.CleanupOK {
		t.Fatalf("%+v", verdict)
	}
	lease, _ := client.Get(context.Background(), leaseName, metav1.GetOptions{})
	if lease.Annotations[maintenanceKey] != "incomplete-drain" {
		t.Fatal("failed cleanup did not fence next run")
	}
}

func TestPreparedNativeResultsProjectWithTrustedHelper(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 required for trusted projector integration")
	}
	target, input, client, _ := lifecycleFixture(t)
	if err := runPreparedLifecycle(context.Background(), client, "ci", target, input); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "public")
	command := exec.Command(python, filepath.Join(fixtureCheckout(t), ".github/scripts/e2e/project.py"), "--private", input.OutputDir, "--public", output, "--source-sha", input.SourceSHA, "--preset", input.Selection)
	if data, err := command.CombinedOutput(); err != nil {
		t.Fatalf("project real native/runtime evidence: %v %s", err, data)
	}
	data, err := os.ReadFile(filepath.Join(output, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var summary struct {
		SourceSHA string `json:"source_sha"`
		Executed  int    `json:"executed"`
		TestsOK   bool   `json:"tests_ok"`
		CleanupOK bool   `json:"cleanup_ok"`
	}
	if err := json.Unmarshal(data, &summary); err != nil {
		t.Fatal(err)
	}
	if summary.SourceSHA != input.SourceSHA || summary.Executed != 1 || !summary.TestsOK || !summary.CleanupOK {
		t.Fatalf("projector rejected native runner evidence: %s", data)
	}
}

func TestPreparedExecutionUsesJSONNotPrivateJUnit(t *testing.T) {
	input, _ := preparedFixture(t)
	if err := os.Mkdir(input.OutputDir, 0700); err != nil {
		t.Fatal(err)
	}
	writeNativeFixture(t, input.OutputDir, types.Report{SuiteSucceeded: true, SpecReports: types.SpecReports{{LeafNodeType: types.NodeTypeIt, State: types.SpecStatePassed}}})
	if err := os.WriteFile(filepath.Join(input.OutputDir, "junit.xml"), []byte("not authoritative XML"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input.GinkgoBinary, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	exit, err := executePrepared(context.Background(), input, []string{})
	if err != nil || exit == nil || *exit != 0 {
		t.Fatalf("valid JSON rejected because of private JUnit: exit=%v err=%v", exit, err)
	}
}

func TestPrivateEvidenceRefusesExistingFilesAndSymlinks(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		sentinel := filepath.Join(t.TempDir(), "existing")
		if err := os.WriteFile(sentinel, []byte("unchanged"), 0600); err != nil {
			t.Fatal(err)
		}
		path := sentinel
		if symlink {
			path = filepath.Join(t.TempDir(), "evidence")
			if err := os.Symlink(sentinel, path); err != nil {
				t.Fatal(err)
			}
		}
		if err := writePrivateFile(path, []byte("replacement")); err == nil {
			t.Fatal("existing evidence path overwritten")
		}
		data, err := os.ReadFile(sentinel)
		if err != nil || string(data) != "unchanged" {
			t.Fatalf("existing file modified: %s %v", data, err)
		}
	}
}

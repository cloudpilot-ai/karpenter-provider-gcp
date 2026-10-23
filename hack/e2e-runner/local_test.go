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
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	schedulingv1 "k8s.io/api/scheduling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

func TestLocalScriptsSourcingIsInert(t *testing.T) {
	dir := t.TempDir()
	for _, tool := range []string{"go", "git", "kubectl", "gcloud", "helm", "ko"} {
		writeTool(t, dir, tool, "#!/bin/sh\necho unexpected tool invocation >&2\nexit 97\n")
	}
	for _, script := range []string{"e2e-deploy.sh", "e2e-clean-env.sh"} {
		path, err := filepath.Abs(filepath.Join("..", script))
		if err != nil {
			t.Fatal(err)
		}
		function := "e2e_deploy"
		if script == "e2e-clean-env.sh" {
			function = "e2e_clean_env"
		}
		cmd := exec.Command("bash", "-c", `set +e +u; set +o pipefail; source "$1"; [[ $- != *e* && $- != *u* ]] || exit 98; [[ $(set -o) == *pipefail*off* ]] || exit 98; declare -F "$2" >/dev/null`, "source-fixture", path, function)
		cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "E2E_PROJECT_ID=", "E2E_LOCATION=")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("source %s: %v\n%s", script, err, output)
		}
	}
}

func TestLocalDeployCreatesValidMissingPriorityClass(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "priorityclass.yaml")
	writeTool(t, dir, "kubectl", fmt.Sprintf(`#!/bin/sh
set -eu
case "$*" in
  'config current-context') echo gke_fixture-pass_us-central1_test-cluster ;;
  'get priorityclass gmp-critical') exit 1 ;;
  'create -f -') cat > %q ;;
  *) exit 97 ;;
esac
`, manifest))
	writeTool(t, dir, "helm", "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("E2E_PROJECT_ID", "fixture-pass")
	t.Setenv("E2E_LOCATION", "us-central1")
	t.Setenv("E2E_CLUSTER_NAME", "test-cluster")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/not-read/adc.json")
	t.Setenv("RELEASE_VERSION", "0.3.2")
	path, err := filepath.Abs("../e2e-deploy.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-e", "-c", `source "$1"; e2e_deploy`, "priority-fixture", path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("local deployment: %v\n%s", err, output)
	}
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var priority schedulingv1.PriorityClass
	if err := yaml.Unmarshal(data, &priority); err != nil {
		t.Fatal(err)
	}
	if priority.Name != "gmp-critical" || priority.Value != 1000000000 || priority.PreemptionPolicy == nil || *priority.PreemptionPolicy != "Never" {
		t.Fatalf("invalid bootstrap PriorityClass: %s", data)
	}
}

func TestLocalCleanupRechecksContextInShellBeforeMutation(t *testing.T) {
	target, _, client, dir := lifecycleFixture(t)
	seedDrainCloud(t, dir, target)
	setLifecycleTargetEnv(t, target)
	serveLeaseBoundary(t, client)
	data, err := os.ReadFile(filepath.Join(dir, "kubectl"))
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `echo gke_fixture-pass_us-central1_test-cluster`, `if [ -f "$f/context-checked" ];then echo wrong-context;else touch "$f/context-checked";echo gke_fixture-pass_us-central1_test-cluster;fi`, 1))
	writeTool(t, dir, "kubectl", string(data))
	cmd := newRunnerCommand()
	cmd.SetArgs([]string{"local-clean-env", "--lock-id", "shell-context"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("local cleanup skipped original shell context check")
	}
	operations, _ := os.ReadFile(filepath.Join(dir, "operations"))
	if strings.Contains(string(operations), "delete ") {
		t.Fatalf("mutated after shell context mismatch: %s", operations)
	}
	lease, _ := client.Get(context.Background(), leaseName, metav1.GetOptions{})
	if lease.Annotations[maintenanceKey] != "incomplete-drain" || lease.Spec.HolderIdentity != nil {
		t.Fatal("failed cleanup did not block and release target")
	}
}

func TestMakeE2ECleanEnvDerivesNonUSRegion(t *testing.T) {
	for _, location := range []string{"asia-southeast1", "asia-southeast1-b"} {
		t.Run(location, func(t *testing.T) {
			target, _, client, dir := lifecycleFixture(t)
			target.Region, target.Location = "asia-southeast1", location
			target.Repository = "asia-southeast1-docker.pkg.dev/fixture-pass/test-images/karpenter"
			setLifecycleTargetEnv(t, target)
			t.Setenv("E2E_REGION", "") // The unchanged Make cleanup recipe does not forward this value.
			serveLeaseBoundary(t, client)
			for _, file := range []string{"cluster", "kubectl"} {
				data, err := os.ReadFile(filepath.Join(dir, file))
				if err != nil {
					t.Fatal(err)
				}
				data = []byte(strings.ReplaceAll(string(data), "us-central1", target.Location))
				writeTool(t, dir, file, string(data))
			}
			cmd := exec.Command("make", "-C", "../..", "e2e-clean-env", "E2E_PROJECT_ID="+target.Project, "E2E_LOCATION="+target.Location, "E2E_PREFIX="+target.Prefix)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("unchanged Make cleanup: %v\n%s", err, output)
			}
			operations, _ := os.ReadFile(filepath.Join(dir, "operations"))
			if !strings.Contains(string(operations), "compute disks list --project fixture-pass") {
				t.Fatalf("cleanup skipped verification: %s", operations)
			}
			lease, _ := client.Get(context.Background(), leaseName, metav1.GetOptions{})
			if lease.Spec.HolderIdentity != nil || lease.Annotations[maintenanceKey] != "" {
				t.Fatal("successful Make cleanup did not release healthy Lease")
			}
		})
	}
}

func TestMakeE2EDeployDrainsBeforeReplacingController(t *testing.T) {
	target, _, client, dir := lifecycleFixture(t)
	seedDrainCloud(t, dir, target)
	for file, content := range map[string]string{
		"deployments.apps.json":                        `{"items":[{"metadata":{"name":"owned","namespace":"karpenter-e2e-test","uid":"deployment-1","labels":{"karpenter-e2e/owned":"true"}}}]}`,
		"gcecustommachinetypes.karpenter.k8s.gcp.json": `{"items":[{"metadata":{"name":"custom","labels":{"karpenter-e2e/owned":"true"}}}]}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	setLifecycleTargetEnv(t, target)
	serveLeaseBoundary(t, client)
	t.Setenv("RELEASE_VERSION", "0.3.2")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/not-read/adc.json")
	namespace, _ := os.ReadFile(filepath.Join(dir, "namespace"))
	binding, _ := os.ReadFile(filepath.Join(dir, "binding"))
	writeTool(t, dir, "helm", fmt.Sprintf(`#!/bin/sh
set -eu
f=%q
[ "$(cat "$f/instances.json")" = '[]' ] && [ "$(cat "$f/disks.json")" = '[]' ] || exit 96
printf 'helm %%s\n' "$*" >> "$f/operations"
`, dir))
	data, err := os.ReadFile(filepath.Join(dir, "kubectl"))
	if err != nil {
		t.Fatal(err)
	}
	writeTool(t, dir, "kubectl", strings.Replace(string(data), `"get priorityclass gmp-critical -o name")`, `"get priorityclass gmp-critical"|"get priorityclass gmp-critical -o name")`, 1))
	cmd := exec.Command("make", "-C", "../..", "e2e-deploy")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("unchanged Make deploy: %v\n%s", err, output)
	}
	operations, _ := os.ReadFile(filepath.Join(dir, "operations"))
	ops := string(operations)
	for _, required := range []string{
		"delete deployments.apps --field-selector=metadata.name=owned --selector=karpenter-e2e/owned=true --cascade=foreground --ignore-not-found --wait=true --timeout=5m",
		"delete gcecustommachinetypes.karpenter.k8s.gcp --selector=karpenter-e2e/owned=true --ignore-not-found --wait=true --timeout=5m",
		"helm upgrade --install karpenter karpenter-gcp/karpenter --version 0.3.2",
	} {
		if !strings.Contains(ops, required) {
			t.Fatalf("missing %q in %s", required, ops)
		}
	}
	// Both cloud inventory and final CR verification must follow deletion and
	// precede even the first Helm operation, not just its controller install.
	beforeHelm, _, _ := strings.Cut(ops, "helm ")
	_, afterDelete, _ := strings.Cut(beforeHelm, "delete gcecustommachinetypes")
	if !strings.Contains(afterDelete, "compute instances list") || !strings.Contains(afterDelete, "compute disks list") || !strings.Contains(afterDelete, "get nodeclaims.karpenter.sh -o json") {
		t.Fatalf("missing completion proof: %s", ops)
	}
	for file, want := range map[string][]byte{"namespace": namespace, "binding": binding} {
		got, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil || string(got) != string(want) {
			t.Fatalf("bootstrap %s changed: %s %v", file, got, err)
		}
	}
	for _, forbidden := range []string{"delete namespace", "create rolebinding", "patch namespace", "label namespace"} {
		if strings.Contains(ops, forbidden) {
			t.Fatalf("bootstrap mutation: %s", ops)
		}
	}
}

func TestLocalDeployBlocksWhenCloudDeletionCannotBeProven(t *testing.T) {
	target, _, client, dir := lifecycleFixture(t)
	seedDrainCloud(t, dir, target)
	setLifecycleTargetEnv(t, target)
	serveLeaseBoundary(t, client)
	// Kubernetes deletion completes, but the subsequent cloud inventory fails.
	data, err := os.ReadFile(filepath.Join(dir, "gcloud"))
	if err != nil {
		t.Fatal(err)
	}
	writeTool(t, dir, "gcloud", strings.Replace(string(data), `"compute instances list "*|"compute disks list "*)`, `"compute instances list "*|"compute disks list "*) if [ -f "$f/nodeclaims.karpenter.sh.json" ] && grep -q '"items":\[\]' "$f/nodeclaims.karpenter.sh.json";then exit 9;fi;`, 1))
	cmd := newRunnerCommand()
	cmd.SetArgs([]string{"local-deploy", "--lock-id", "unproven-cloud"})
	err = cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "cloud deletion unconfirmed") {
		t.Fatalf("unproven cloud deletion accepted: %v", err)
	}
	operations, _ := os.ReadFile(filepath.Join(dir, "operations"))
	if strings.Contains(string(operations), "upgrade ") || strings.Contains(string(operations), "configure-docker") {
		t.Fatalf("replaced old controller before cloud proof: %s", operations)
	}
	lease, _ := client.Get(context.Background(), leaseName, metav1.GetOptions{})
	if lease.Annotations[maintenanceKey] != "incomplete-drain" || lease.Spec.HolderIdentity != nil {
		t.Fatal("unproven drain did not block target")
	}
}

func TestLocalCleanupFailureStopsShellAndMarksMaintenance(t *testing.T) {
	target, _, client, dir := lifecycleFixture(t)
	seedDrainCloud(t, dir, target)
	setLifecycleTargetEnv(t, target)
	serveLeaseBoundary(t, client)
	data, err := os.ReadFile(filepath.Join(dir, "kubectl"))
	if err != nil {
		t.Fatal(err)
	}
	writeTool(t, dir, "kubectl", strings.Replace(string(data), `"delete node"*|"delete gcenode"*|"delete gcecustom"*)`, `"delete node"*|"delete gcenode"*|"delete gcecustom"*) exit 9;`, 1))
	cmd := newRunnerCommand()
	cmd.SetArgs([]string{"local-deploy", "--lock-id", "failed-shell"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("shell cleanup failure accepted")
	}
	operations, _ := os.ReadFile(filepath.Join(dir, "operations"))
	if strings.Count(string(operations), "delete ") != 1 || strings.Contains(string(operations), "upgrade ") {
		t.Fatalf("shell continued after failed deletion: %s", operations)
	}
	lease, _ := client.Get(context.Background(), leaseName, metav1.GetOptions{})
	if lease.Annotations[maintenanceKey] != "incomplete-drain" || lease.Spec.HolderIdentity != nil {
		t.Fatal("failed cleanup did not block and release target")
	}
}

func TestLocalCleanupCancellationStopsDescendantsWithoutRetry(t *testing.T) {
	for _, mode := range []string{"canceled", "uncertain"} {
		t.Run(mode, func(t *testing.T) {
			target, _, client, dir := lifecycleFixture(t)
			seedDrainCloud(t, dir, target)
			setLifecycleTargetEnv(t, target)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err := withOwnedLease(ctx, client, "local-process", true, func(run context.Context, owner *leaseOwner) error {
				callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					if mode == "canceled" {
						cancel()
					} else {
						client.mu.Lock()
						client.lease.Annotations[leaseTokenKey] = "replacement"
						client.mu.Unlock()
						if err := owner.confirm(run); err == nil {
							t.Error("replacement token accepted")
						}
					}
				}))
				defer callback.Close()
				data, err := os.ReadFile(filepath.Join(dir, "kubectl"))
				if err != nil {
					t.Fatal(err)
				}
				replacement := fmt.Sprintf(`"delete node"*|"delete gcenode"*|"delete gcecustom"*) sleep 60 & echo $! > "$f/child-pid";curl -fsS %q >/dev/null;wait;`, callback.URL)
				writeTool(t, dir, "kubectl", strings.Replace(string(data), `"delete node"*|"delete gcenode"*|"delete gcecustom"*)`, replacement, 1))
				return runLocalLifecycle(run, owner, target, true)
			})
			if err == nil {
				t.Fatal("interrupted cleanup succeeded")
			}
			pid, readErr := os.ReadFile(filepath.Join(dir, "child-pid"))
			if readErr != nil {
				t.Fatal("shell descendant did not start:", readErr)
			}
			child, parseErr := strconv.Atoi(strings.TrimSpace(string(pid)))
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			// Zombies cannot mutate; the parent may already have reaped the child.
			state, psErr := exec.Command("ps", "-p", strconv.Itoa(child), "-o", "stat=").Output()
			if psErr != nil {
				var exited *exec.ExitError
				if !errors.As(psErr, &exited) || exited.ExitCode() != 1 {
					t.Fatalf("inspect cleanup descendant: %v", psErr)
				}
			} else if !strings.HasPrefix(strings.TrimSpace(string(state)), "Z") {
				_ = syscall.Kill(child, syscall.SIGKILL)
				t.Fatalf("cleanup descendant still running: %s", state)
			}
			operations, _ := os.ReadFile(filepath.Join(dir, "operations"))
			if strings.Count(string(operations), "delete ") != 1 || strings.Contains(string(operations), "upgrade ") {
				t.Fatalf("cleanup retried or deployed after interruption: %s", operations)
			}
			lease, _ := client.Get(context.Background(), leaseName, metav1.GetOptions{})
			if mode == "canceled" {
				if lease.Annotations[maintenanceKey] != "incomplete-drain" || lease.Spec.HolderIdentity != nil {
					t.Fatal("canceled cleanup did not block and release target")
				}
			} else if lease.Annotations[maintenanceKey] != "" || lease.Spec.HolderIdentity == nil || lease.Annotations[leaseTokenKey] != "replacement" {
				t.Fatal("mutated Lease after ownership uncertainty")
			}
		})
	}
}

func TestPreparedTargetDoesNotDeriveMissingRegion(t *testing.T) {
	target, _, _, _ := lifecycleFixture(t)
	setLifecycleTargetEnv(t, target)
	t.Setenv("E2E_REGION", "")
	if _, err := loadLifecycleTarget(true); err == nil || !strings.Contains(err.Error(), "missing E2E_REGION") {
		t.Fatalf("CI inferred region: %v", err)
	}
}

func TestLocalCleanupRejectsUnknownInventoryBeforeShellMutation(t *testing.T) {
	for _, resource := range []string{"nodeclaims.karpenter.sh", "deployments.apps"} {
		t.Run(resource, func(t *testing.T) {
			target, _, client, dir := lifecycleFixture(t)
			seedDrainCloud(t, dir, target)
			setLifecycleTargetEnv(t, target)
			serveLeaseBoundary(t, client)
			if err := os.WriteFile(filepath.Join(dir, resource+".json"), []byte(`{"items":[{"metadata":{"name":"legacy","namespace":"karpenter-e2e-test","uid":"legacy"}}]}`), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := newRunnerCommand()
			cmd.SetArgs([]string{"local-clean-env", "--lock-id", "unknown-inventory"})
			if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "unknown") {
				t.Fatalf("unknown inventory accepted: %v", err)
			}
			operations, _ := os.ReadFile(filepath.Join(dir, "operations"))
			if strings.Contains(string(operations), "delete ") {
				t.Fatalf("shell mutated unknown inventory: %s", operations)
			}
			lease, _ := client.Get(context.Background(), leaseName, metav1.GetOptions{})
			if lease.Annotations[maintenanceKey] != "incomplete-drain" {
				t.Fatal("unknown inventory did not block target")
			}
		})
	}
}

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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/yaml"
)

func TestPreparedChartImageReference(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm required to render the real controller chart")
	}
	for _, digest := range []string{"", "sha256:" + strings.Repeat("a", 64)} {
		t.Run(digest, func(t *testing.T) {
			repository := "example.invalid/controller"
			command := exec.Command(helm, "template", "karpenter", "../../charts/karpenter", "--show-only", "templates/deployment.yaml", "--set-string", "controller.image.repository="+repository, "--set-string", "controller.image.tag=prepared", "--set-string", "controller.image.digest="+digest)
			data, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("render actual chart: %v\n%s", err, data)
			}
			var deployment appsv1.Deployment
			if err := yaml.Unmarshal(data, &deployment); err != nil {
				t.Fatal(err)
			}
			want := repository + ":prepared"
			if digest != "" {
				want = repository + "@" + digest
			}
			if len(deployment.Spec.Template.Spec.Containers) != 1 || deployment.Spec.Template.Spec.Containers[0].Image != want {
				t.Fatalf("chart must render prepared identity %q: %+v", want, deployment.Spec.Template.Spec.Containers)
			}
		})
	}
}

func TestOwnedLeaseCancellationRetainsCleanupOwnership(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &memoryLeaseClient{}
	err := withOwnedLease(ctx, client, "ci", false, func(run context.Context, owner *leaseOwner) error {
		cancel()
		<-run.Done()
		cleanup, stop := owner.cleanupContext(time.Second)
		defer stop()
		if cleanup.Err() != nil {
			t.Fatalf("test cancellation stopped heartbeat lifetime: %v", cleanup.Err())
		}
		if err := owner.confirm(cleanup); err != nil {
			t.Fatal(err)
		}
		if _, err := acquireLease(cleanup, client, "local", false); err == nil {
			t.Fatal("local deploy acquired during cleanup")
		}
		return run.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	if _, err := acquireLease(context.Background(), client, "next", false); err != nil {
		t.Fatal(err)
	}
}

func TestOwnedLeaseLossProhibitsCleanupAndRelease(t *testing.T) {
	for _, mode := range []string{"stolen", "unavailable", "expired"} {
		t.Run(mode, func(t *testing.T) {
			client := &failingLeaseBoundary{memoryLeaseClient: &memoryLeaseClient{}}
			err := withOwnedLease(context.Background(), client, "ci", false, func(run context.Context, owner *leaseOwner) error {
				client.mu.Lock()
				switch mode {
				case "stolen":
					client.lease.Annotations[leaseTokenKey] = "other"
				case "unavailable":
					client.unavailable = true
				case "expired":
					client.lease.Spec.RenewTime = ptr(metav1.NewMicroTime(time.Now().Add(-time.Hour)))
				}
				client.mu.Unlock()
				if err := owner.confirm(run); err == nil {
					t.Fatal("ownership uncertainty accepted")
				}
				<-run.Done()
				cleanup, stop := owner.cleanupContext(time.Second)
				defer stop()
				if cleanup.Err() == nil {
					t.Fatal("cleanup allowed after loss")
				}
				return nil
			})
			if err == nil {
				t.Fatal("loss not returned")
			}
			client.mu.Lock()
			defer client.mu.Unlock()
			if client.lease.Spec.HolderIdentity == nil {
				t.Fatal("lease released after uncertainty")
			}
		})
	}
}

type failingLeaseBoundary struct {
	*memoryLeaseClient
	unavailable bool
	failUpdate  bool
}

func (c *failingLeaseBoundary) Update(ctx context.Context, l *coordinationv1.Lease, opts metav1.UpdateOptions) (*coordinationv1.Lease, error) {
	c.mu.Lock()
	fail := c.failUpdate
	c.mu.Unlock()
	if fail {
		return nil, fmt.Errorf("renewal CAS unavailable")
	}
	return c.memoryLeaseClient.Update(ctx, l, opts)
}

func (c *failingLeaseBoundary) Get(ctx context.Context, name string, opts metav1.GetOptions) (*coordinationv1.Lease, error) {
	c.mu.Lock()
	fail := c.unavailable
	c.mu.Unlock()
	if fail {
		return nil, fmt.Errorf("API unavailable")
	}
	return c.memoryLeaseClient.Get(ctx, name, opts)
}

func TestPreparedTargetRequiresExplicitConfiguration(t *testing.T) {
	for _, key := range []string{"E2E_PROJECT_ID", "E2E_LOCATION", "E2E_REGION", "E2E_PREFIX", "E2E_CLUSTER_NAME", "E2E_IMAGE_REPOSITORY", "E2E_CONTROLLER_SERVICE_ACCOUNT", "E2E_NODE_SERVICE_ACCOUNT", "E2E_PODS_RANGE_NAME", "E2E_SMALL_PODS_RANGE_NAME", "E2E_LARGE_PODS_RANGE_NAME", "E2E_RESOURCE_MANAGER_TAG_KEY"} {
		t.Setenv(key, "")
	}
	if _, err := loadLifecycleTarget(true); err == nil {
		t.Fatal("missing CI target accepted")
	}
}

func TestDrainRejectsUnknownResourcesBeforeMutation(t *testing.T) {
	target, _, client, dir := lifecycleFixture(t)
	if err := os.WriteFile(filepath.Join(dir, "resources"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nodeclaims.karpenter.sh.json"), []byte(`{"items":[{"metadata":{"name":"legacy"}}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	err := withOwnedLease(context.Background(), client, "ci", true, func(ctx context.Context, o *leaseOwner) error {
		return drainOwned(ctx, o, target)
	})
	if err == nil || !strings.Contains(err.Error(), "unknown nodeclaims.karpenter.sh") {
		t.Fatalf("%v", err)
	}
	operations, _ := os.ReadFile(filepath.Join(dir, "operations"))
	if strings.Contains(string(operations), "delete ") {
		t.Fatalf("mutated before ownership inventory: %s", operations)
	}
}
func writeTool(t *testing.T, dir, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedLifecycleTargetMismatchCannotMutate(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "kubectl", "#!/bin/sh\n[ \"$*\" = 'config current-context' ] || exit 97\necho wrong-cluster\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	b, s, image := preparedFixture(t)
	input := preparedExecution{Bundle: b, Source: s, Image: image, Workers: 20, OutputDir: filepath.Join(t.TempDir(), "result")}
	err := runPreparedLifecycle(context.Background(), &memoryLeaseClient{}, "ci", lifecycleTarget{Project: "project", Location: "us-central1", Cluster: "cluster", Repository: strings.SplitN(image, "@", 2)[0]}, input)
	if err == nil || !strings.Contains(err.Error(), "kubeconfig context mismatch") {
		t.Fatalf("wrong target not checked: %v", err)
	}
	data, readErr := os.ReadFile(filepath.Join(input.OutputDir, "outcome.json"))
	if readErr != nil || !strings.Contains(string(data), `"infrastructure": "failed"`) {
		t.Fatalf("missing failed lifecycle: %s %v", data, readErr)
	}
}

func lifecycleFixture(t *testing.T) (lifecycleTarget, preparedExecution, *memoryLeaseClient, string) {
	t.Helper()
	b, s, image := preparedFixture(t)
	target := lifecycleTarget{Project: "fixture-pass", Region: "us-central1", Location: "us-central1", Prefix: "test", Cluster: "test-cluster", Repository: "us-central1-docker.pkg.dev/fixture-pass/test-images/karpenter", ControllerSA: "controller@fixture-pass.iam.gserviceaccount.com", NodeSA: "nodes@fixture-pass.iam.gserviceaccount.com", Pods: "pods", SmallPods: "small", LargePods: "large", TagKey: "test-tag"}
	image = target.Repository + "@" + strings.SplitN(image, "@", 2)[1]
	fakeController(t, image, strings.SplitN(image, "@", 2)[1], s.HeadSHA)
	dir := t.TempDir()
	header := fmt.Sprintf("#!/bin/sh\nset -eu\nf=%q\nprintf '%%s\\n' \"$*\" >> \"$f/operations\"\n", dir)
	for name, data := range map[string]string{
		"cluster":   `{"name":"test-cluster","location":"us-central1","selfLink":"https://container.googleapis.com/v1/projects/fixture-pass/locations/us-central1/clusters/test-cluster","endpoint":"127.0.0.1","subnetwork":"test-subnet","ipAllocationPolicy":{"clusterSecondaryRangeName":"pods","additionalPodRangesConfig":{"podRangeNames":["small","large"]}}}`,
		"subnet":    `{"secondaryIpRanges":[{"rangeName":"pods","ipCidrRange":"10.0.0.0/16"},{"rangeName":"small","ipCidrRange":"10.1.0.0/22"},{"rangeName":"large","ipCidrRange":"10.2.0.0/20"}]}`,
		"digest":    strings.SplitN(image, "@", 2)[1],
		"namespace": `{"metadata":{"name":"karpenter-e2e-test","uid":"bootstrap-namespace","labels":{"karpenter-e2e/owned":"true"}}}`,
		"binding":   `{"metadata":{"name":"e2e-runtime-test-workloads","namespace":"karpenter-e2e-test","uid":"bootstrap-binding"},"roleRef":{"apiGroup":"rbac.authorization.k8s.io","kind":"ClusterRole","name":"e2e-runtime-test-workloads"},"subjects":[{"kind":"User","apiGroup":"rbac.authorization.k8s.io","name":"test-ci-sa@fixture-pass.iam.gserviceaccount.com"}]}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeTool(t, dir, "kubectl", header+`
case "$*" in
 "config current-context") echo gke_fixture-pass_us-central1_test-cluster ;;
 "config view --minify -o json") echo '{"clusters":[{"cluster":{"server":"https://127.0.0.1"}}]}' ;;
 "get namespace karpenter-system -o name") echo namespace/karpenter-system ;;
 "get priorityclass gmp-critical -o name") [ ! -f "$f/missing-priority" ] || exit 8; echo priorityclass/gmp-critical ;;
 "get crd "*) if [ -f "$f/resources" ];then echo crd;fi ;;
 "get node"*|"get gcenode"*|"get gcecustom"*) if [ -f "$f/$2.json" ];then cat "$f/$2.json";else echo '{"items":[]}';fi ;;
 "delete node"*|"delete gcenode"*|"delete gcecustom"*) echo '{"items":[]}' > "$f/$2.json";if [ "$2" = nodeclaims.karpenter.sh ] && [ ! -f "$f/sticky-cloud" ];then echo '[]' > "$f/instances.json";echo '[]' > "$f/disks.json";fi ;;
 "get namespace karpenter-e2e-test -o json --ignore-not-found") if [ -f "$f/namespace" ];then cat "$f/namespace";fi ;;
 "-n karpenter-e2e-test get rolebinding e2e-runtime-test-workloads -o json") cat "$f/binding" ;;
 "-n karpenter-e2e-test get deployments.apps -o json"|"-n karpenter-e2e-test get replicasets.apps -o json"|"-n karpenter-e2e-test get pods -o json") if [ -f "$f/$4.json" ];then cat "$f/$4.json";else echo '{"items":[]}';fi ;;
 "-n karpenter-e2e-test delete deployments.apps "*) [ ! -f "$f/incomplete" ] || exit 9;for kind in deployments.apps replicasets.apps pods;do echo '{"items":[]}' > "$f/$kind.json";done ;;
 *"rollout status"*) exit 0 ;;
 *"get deployment"*) cat "$CONTROLLER_FIXTURE/deployment" ;;
 *"get pods"*) cat "$CONTROLLER_FIXTURE/pods" ;;
 *"logs"*) cat "$CONTROLLER_FIXTURE/logs" ;;
 *) exit 97 ;;
esac
`)
	writeTool(t, dir, "gcloud", header+`
case "$*" in
 "container clusters describe "*) cat "$f/cluster" ;;
 "compute networks subnets describe "*) cat "$f/subnet" ;;
 "resource-manager tags keys describe "*) key=${5#fixture-pass/};printf '{"name":"tagKeys/123","shortName":"%s"}' "$key" ;;
 "resource-manager tags values list "*) echo '[{"parent":"tagKeys/123","shortName":"e2e"}]' ;;
 "compute instances list "*|"compute disks list "*) if [ -f "$f/$2.json" ];then cat "$f/$2.json";else echo '[]';fi ;;
 *) exit 97 ;;
esac
`)
	writeTool(t, dir, "docker-credential-gcloud", "#!/bin/sh\nexit 97\n")
	writeTool(t, dir, "crane", header+`
case "$1" in
 push) [ -f "$2/index.json" ]; [ -d "$2/blobs/sha256" ];[ "$3" = '`+target.Repository+`:e2e-`+s.HeadSHA+`-9' ] ;;
 digest) cat "$f/digest" ;;
 *) exit 97 ;;
esac
`)
	writeTool(t, dir, "helm", header+`
[ "$1 $2" = 'upgrade --install' ]
[ -f "$4" ]
case "$3" in
 karpenter-crd) [ "$(cat "$4")" = 'previously validated CRDs' ] ;;
 karpenter) [ "$(cat "$4")" = 'previously validated chart' ];case "$*" in *"controller.image.digest=$(cat "$f/digest")"*) ;; *) exit 96;;esac ;;
 *) exit 97 ;;
esac
[ ! -f "$f/fail-helm" ] || exit 10
if [ -f "$f/incomplete" ];then echo '{"items":[{"metadata":{"name":"owned","namespace":"karpenter-e2e-test","uid":"deployment-1","labels":{"karpenter-e2e/owned":"true"}}}]}' > "$f/deployments.apps.json";fi
if [ -f "$f/callback" ];then curl -fsS "$(cat "$f/callback")" >/dev/null;fi
`)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	client := &memoryLeaseClient{lease: &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: leaseName, ResourceVersion: "1"}}}
	return target, preparedExecution{Bundle: b, Source: s, Image: image, Workers: 20, OutputDir: filepath.Join(t.TempDir(), "results")}, client, dir
}

func TestPreparedLifecycleUsesPreparedImageChartsAndNativeTests(t *testing.T) {
	target, input, client, dir := lifecycleFixture(t)
	if err := runPreparedLifecycle(context.Background(), client, "ci", target, input); err != nil {
		t.Fatal(err)
	}
	outcome := readLifecycleOutcome(t, input.OutputDir)
	if outcome.Overall() != statePassed || outcome.Counts.Executed != 1 || outcome.Cleanup != statePassed {
		t.Fatalf("%+v", outcome)
	}
	if _, err := os.Stat(filepath.Join(dir, "namespace")); err != nil {
		t.Fatal("bootstrap namespace was not preserved")
	}
	data, err := os.ReadFile(filepath.Join(dir, "operations"))
	if err != nil {
		t.Fatal(err)
	}
	operations := string(data)
	for _, arg := range []string{"push ", "karpenter-crd " + filepath.Join(input.Bundle.Root, "karpenter-crd.tgz"), "karpenter " + filepath.Join(input.Bundle.Root, "karpenter.tgz"), "rollout status"} {
		if !strings.Contains(operations, arg) {
			t.Fatalf("missing %q in %s", arg, operations)
		}
	}
	if strings.Contains(operations, "dependency build") || strings.Contains(operations, "configure-docker") {
		t.Fatal("runtime built source or ran local scripts")
	}
	if _, err := acquireLease(context.Background(), client, "local", false); err != nil {
		t.Fatal(err)
	}
}
func readLifecycleOutcome(t *testing.T, dir string) runOutcome {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "outcome.json"))
	if err != nil {
		t.Fatal(err)
	}
	var outcome runOutcome
	if err := json.Unmarshal(data, &outcome); err != nil {
		t.Fatal(err)
	}
	return outcome
}

func TestPreparedLifecycleIncompleteCleanupPreservesTestOutcome(t *testing.T) {
	target, input, client, dir := lifecycleFixture(t)
	if err := os.WriteFile(filepath.Join(dir, "incomplete"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	err := runPreparedLifecycle(context.Background(), client, "ci", target, input)
	outcome := readLifecycleOutcome(t, input.OutputDir)
	if err == nil || outcome.Tests != statePassed || outcome.Cleanup != stateIncomplete || outcome.Overall() == statePassed {
		t.Fatalf("%+v: %v", outcome, err)
	}
}

func TestPreparedLifecycleCancellationCleansWithSeparateOwnership(t *testing.T) {
	target, input, client, dir := lifecycleFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := acquireLease(context.Background(), client, "local", false); err == nil {
			t.Error("local acquired during prepared test")
		}
		cancel()
		close(called)
	}))
	defer server.Close()
	target.TagKey = server.URL
	err := runPreparedLifecycle(ctx, client, "ci", target, input)
	select {
	case <-called:
	default:
		t.Fatal("prepared native test did not run")
	}
	outcome := readLifecycleOutcome(t, input.OutputDir)
	if err == nil || outcome.Tests != stateCancelled || outcome.Cleanup != statePassed {
		t.Fatalf("%+v: %v", outcome, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "namespace")); err != nil {
		t.Fatal("cancellation deleted bootstrap namespace")
	}
	if _, err := acquireLease(context.Background(), client, "local-after", false); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedLifecycleRefusesUnlabeledNamespace(t *testing.T) {
	target, input, client, dir := lifecycleFixture(t)
	if err := os.WriteFile(filepath.Join(dir, "namespace"), []byte(`{"metadata":{"name":"karpenter-e2e-test"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	err := runPreparedLifecycle(context.Background(), client, "ci", target, input)
	if err == nil || !strings.Contains(err.Error(), "maintenance-required") {
		t.Fatalf("%v", err)
	}
	operations, _ := os.ReadFile(filepath.Join(dir, "operations"))
	for _, mutation := range []string{"push ", "upgrade ", "delete ", "create "} {
		if strings.Contains(string(operations), mutation) {
			t.Fatalf("mutated unknown namespace: %s", operations)
		}
	}
}

func TestPreparedLifecycleDigestMismatchStopsBeforeDeploy(t *testing.T) {
	target, input, client, dir := lifecycleFixture(t)
	if err := os.WriteFile(filepath.Join(dir, "digest"), []byte("sha256:"+strings.Repeat("0", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runPreparedLifecycle(context.Background(), client, "ci", target, input); err == nil {
		t.Fatal("wrong remote digest accepted")
	}
	operations, _ := os.ReadFile(filepath.Join(dir, "operations"))
	if strings.Contains(string(operations), "upgrade --install") {
		t.Fatal("deployed wrong remote digest")
	}
}

func TestPreparedLifecycleHeartbeatLossTerminatesTestsWithoutCleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("exercises real one-minute heartbeat")
	}
	for _, mode := range []string{"stolen", "failed-renewal"} {
		t.Run(mode, func(t *testing.T) {
			target, input, client, dir := lifecycleFixture(t)
			boundary := &failingLeaseBoundary{memoryLeaseClient: client}
			called := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				client.mu.Lock()
				if mode == "stolen" {
					client.lease.Annotations[leaseTokenKey] = "thief"
				} else {
					boundary.failUpdate = true
				}
				client.mu.Unlock()
				close(called)
			}))
			defer server.Close()
			target.TagKey = server.URL
			ctx, cancel := context.WithTimeout(context.Background(), 85*time.Second)
			defer cancel()
			err := runPreparedLifecycle(ctx, boundary, "ci", target, input)
			select {
			case <-called:
			default:
				t.Fatal("native tests not executed")
			}
			outcome := readLifecycleOutcome(t, input.OutputDir)
			if err == nil || !strings.Contains(err.Error(), "maintenance-required") || outcome.Tests != stateCancelled || outcome.Cleanup != stateIncomplete {
				t.Fatalf("%+v: %v", outcome, err)
			}
			if _, err := os.Stat(filepath.Join(dir, "namespace")); err != nil {
				t.Fatal("mutated namespace after loss")
			}
			operations, _ := os.ReadFile(filepath.Join(dir, "operations"))
			if strings.Contains(string(operations), "delete deployments.apps") {
				t.Fatal("workload cleanup mutation after lease loss")
			}
			client.mu.Lock()
			defer client.mu.Unlock()
			if client.lease.Spec.HolderIdentity == nil {
				t.Fatal("released after uncertainty")
			}
		})
	}
}

func seedDrainCloud(t *testing.T, dir string, target lifecycleTarget) {
	t.Helper()
	instance := "https://www.googleapis.com/compute/v1/projects/fixture-pass/zones/us-central1-a/instances/karpenter-owned"
	disk := "https://www.googleapis.com/compute/v1/projects/fixture-pass/zones/us-central1-a/disks/karpenter-disk"
	files := map[string]any{
		"resources":                             "installed",
		"nodepools.karpenter.sh.json":           map[string]any{"items": []any{map[string]any{"metadata": map[string]any{"name": "pool", "labels": map[string]string{ownerLabel: "true"}}}}},
		"gcenodeclasses.karpenter.k8s.gcp.json": map[string]any{"items": []any{map[string]any{"metadata": map[string]any{"name": "class", "labels": map[string]string{ownerLabel: "true"}}}}},
		"nodeclaims.karpenter.sh.json":          map[string]any{"items": []any{map[string]any{"metadata": map[string]any{"name": "claim", "labels": map[string]string{ownerLabel: "true"}}, "status": map[string]string{"providerID": "gce://fixture-pass/us-central1-a/karpenter-owned"}}}},
		"instances.json":                        []any{map[string]any{"id": "12345", "name": "karpenter-owned", "selfLink": instance, "zone": "https://www.googleapis.com/compute/v1/projects/fixture-pass/zones/us-central1-a", "labels": map[string]string{"karpenter-sh-nodepool": "pool", "karpenter-k8s-gcp-gcenodeclass": "class", "goog-k8s-cluster-name": target.Cluster, "goog-k8s-cluster-location": target.Location}, "disks": []any{map[string]any{"source": disk, "autoDelete": true}}}},
		"disks.json":                            []any{map[string]any{"id": "56789", "name": "karpenter-disk", "selfLink": disk}},
	}
	for name, value := range files {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPreparedLifecycleDrainsOwnedCloudBeforePushing(t *testing.T) {
	target, input, client, dir := lifecycleFixture(t)
	seedDrainCloud(t, dir, target)
	if err := runPreparedLifecycle(context.Background(), client, "ci", target, input); err != nil {
		t.Fatal(err)
	}
	operations, _ := os.ReadFile(filepath.Join(dir, "operations"))
	ops := string(operations)
	if strings.Index(ops, "delete nodepools") > strings.Index(ops, "delete nodeclaims") || strings.Index(ops, "delete nodeclaims") > strings.Index(ops, "push ") {
		t.Fatalf("wrong drain/deploy order: %s", ops)
	}
}

func TestOwnedCloudDeletionMustCompleteWithinCleanupBound(t *testing.T) {
	target, _, client, dir := lifecycleFixture(t)
	seedDrainCloud(t, dir, target)
	if err := os.WriteFile(filepath.Join(dir, "sticky-cloud"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	err := withOwnedLease(context.Background(), client, "ci", true, func(ctx context.Context, o *leaseOwner) error {
		cleanup, stop := o.cleanupContext(time.Second)
		defer stop()
		return drainOwned(cleanup, o, target)
	})
	if err == nil || !strings.Contains(err.Error(), "maintenance-required") {
		t.Fatalf("cloud deletion unconfirmed: %v", err)
	}
}

func TestIncompleteCleanupBlocksEveryAcquisitionAfterRelease(t *testing.T) {
	target, input, client, dir := lifecycleFixture(t)
	if err := os.WriteFile(filepath.Join(dir, "incomplete"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := runPreparedLifecycle(context.Background(), client, "ci", target, input); err == nil {
		t.Fatal("incomplete cleanup passed")
	}
	lease, err := client.Get(context.Background(), leaseName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Annotations[maintenanceKey] != "incomplete-drain" || lease.Spec.HolderIdentity != nil {
		t.Fatalf("marker not preserved across release: %+v", lease)
	}
	for _, bootstrap := range []bool{false, true} {
		if _, err := acquireLease(context.Background(), client, "local-or-ci", bootstrap); err == nil {
			t.Fatal("blocked target acquired")
		}
	}
}

func TestStaleOwnerCannotMarkOrClearMaintenance(t *testing.T) {
	client := &memoryLeaseClient{}
	err := withOwnedLease(context.Background(), client, "ci", false, func(ctx context.Context, owner *leaseOwner) error {
		client.mu.Lock()
		client.lease.Annotations[leaseTokenKey] = "replacement"
		client.lease.Annotations[maintenanceKey] = "incomplete-drain"
		client.mu.Unlock()
		if err := owner.requireMaintenance(ctx); err == nil {
			t.Fatal("stale owner marked target")
		}
		if err := owner.lease.release(ctx); err == nil {
			t.Fatal("stale owner released target")
		}
		return nil
	})
	if err == nil {
		t.Fatal("loss not returned")
	}
	lease, _ := client.Get(context.Background(), leaseName, metav1.GetOptions{})
	if lease.Annotations[maintenanceKey] != "incomplete-drain" || lease.Annotations[leaseTokenKey] != "replacement" {
		t.Fatal("stale holder mutated replacement")
	}
}

func TestLifecycleRequiresBootstrapAndRefusesAbandonedLease(t *testing.T) {
	for _, client := range []*memoryLeaseClient{{}, {lease: &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: leaseName, ResourceVersion: "1"}, Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr("dead-host"), RenewTime: ptr(metav1.NewMicroTime(time.Now().Add(-time.Hour))), LeaseDurationSeconds: ptr(int32(600))}}}} {
		if err := withOwnedLease(context.Background(), client, "ci", true, func(context.Context, *leaseOwner) error { t.Fatal("run started without safe bootstrap"); return nil }); err == nil {
			t.Fatal("missing/abandoned bootstrap accepted")
		}
	}
}

func TestDefaultTestOnlyRunnerRejectsAbandonedLease(t *testing.T) {
	client := &memoryLeaseClient{lease: &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: leaseName, ResourceVersion: "1"}, Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr("dead-host"), RenewTime: ptr(metav1.NewMicroTime(time.Now().Add(-time.Hour))), LeaseDurationSeconds: ptr(int32(600))}}}
	err := withLease(context.Background(), client, "local-test", func(context.Context) error { t.Fatal("automatically recovered abandoned shared target"); return nil })
	if err == nil {
		t.Fatal("abandoned Lease accepted")
	}
}

func setLifecycleTargetEnv(t *testing.T, target lifecycleTarget) {
	t.Helper()
	for k, v := range map[string]string{"E2E_PROJECT_ID": target.Project, "E2E_LOCATION": target.Location, "E2E_REGION": target.Region, "E2E_PREFIX": target.Prefix, "E2E_CLUSTER_NAME": target.Cluster, "E2E_IMAGE_REPOSITORY": target.Repository, "E2E_CONTROLLER_SERVICE_ACCOUNT": target.ControllerSA, "E2E_NODE_SERVICE_ACCOUNT": target.NodeSA, "E2E_PODS_RANGE_NAME": target.Pods, "E2E_SMALL_PODS_RANGE_NAME": target.SmallPods, "E2E_LARGE_PODS_RANGE_NAME": target.LargePods, "E2E_RESOURCE_MANAGER_TAG_KEY": target.TagKey} {
		t.Setenv(k, v)
	}
}

func serveLeaseBoundary(t *testing.T, client *memoryLeaseClient) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/coordination.k8s.io/v1/namespaces/karpenter-system/leases/karpenter-e2e-tests" {
			t.Errorf("noncanonical shared Lease path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			lease, err := client.Get(r.Context(), leaseName, metav1.GetOptions{})
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			lease.TypeMeta = metav1.TypeMeta{APIVersion: "coordination.k8s.io/v1", Kind: "Lease"}
			_ = json.NewEncoder(w).Encode(lease)
		case http.MethodPut:
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			var lease coordinationv1.Lease
			if _, _, err := scheme.Codecs.UniversalDeserializer().Decode(data, nil, &lease); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			saved, err := client.Update(r.Context(), &lease, metav1.UpdateOptions{})
			if err != nil {
				t.Error(err)
				w.WriteHeader(409)
				return
			}
			saved.TypeMeta = metav1.TypeMeta{APIVersion: "coordination.k8s.io/v1", Kind: "Lease"}
			_ = json.NewEncoder(w).Encode(saved)
		default:
			t.Errorf("unexpected Lease method %s", r.Method)
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(server.Close)
	file := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(file, []byte(fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: fixture\n  cluster:\n    server: %s\ncontexts:\n- name: fixture\n  context:\n    cluster: fixture\ncurrent-context: fixture\n", server.URL)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", file)
}

func TestLocalDeployAndCleanupContendWithPreparedOwner(t *testing.T) {
	target, _, client, dir := lifecycleFixture(t)
	setLifecycleTargetEnv(t, target)
	serveLeaseBoundary(t, client)
	if err := withOwnedLease(context.Background(), client, "prepared-ci", true, func(context.Context, *leaseOwner) error {
		for _, script := range []string{"e2e-deploy.sh", "e2e-clean-env.sh"} {
			cmd := exec.Command("bash", filepath.Join("..", script), "--lock-id", "local-user")
			output, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(output), "prepared-ci") {
				t.Fatalf("%s: %v\n%s", script, err, output)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ops, _ := os.ReadFile(filepath.Join(dir, "operations"))
	if strings.Contains(string(ops), "push ") || strings.Contains(string(ops), "upgrade --install") || strings.Contains(string(ops), "delete ") {
		t.Fatalf("local mutation during CI: %s", ops)
	}
}

func TestExplicitLocalReleaseDeployRetainsPublishedChartMode(t *testing.T) {
	target, _, client, dir := lifecycleFixture(t)
	setLifecycleTargetEnv(t, target)
	serveLeaseBoundary(t, client)
	t.Setenv("RELEASE_VERSION", "0.3.2")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/not-read/adc.json")
	writeTool(t, dir, "helm", fmt.Sprintf("#!/bin/sh\nprintf 'helm %%s\\n' \"$*\" >> %q\n", filepath.Join(dir, "operations")))
	// Explicit local shell retains its original priority-class existence check.
	kubectl, err := os.ReadFile(filepath.Join(dir, "kubectl"))
	if err != nil {
		t.Fatal(err)
	}
	kubectl = []byte(strings.Replace(string(kubectl), `"get priorityclass gmp-critical -o name")`, `"get priorityclass gmp-critical"|"get priorityclass gmp-critical -o name")`, 1))
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), kubectl, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := newRunnerCommand()
	cmd.SetArgs([]string{"local-deploy", "--lock-id", "local-release"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	ops, _ := os.ReadFile(filepath.Join(dir, "operations"))
	for _, arg := range []string{"repo add karpenter-gcp", "karpenter-crd karpenter-gcp/karpenter-crd --version 0.3.2", "karpenter karpenter-gcp/karpenter --version 0.3.2"} {
		if !strings.Contains(string(ops), arg) {
			t.Fatalf("missing %q in %s", arg, ops)
		}
	}
	if strings.Contains(string(ops), "dependency build") || strings.Contains(string(ops), "push ") {
		t.Fatal("release mode built source")
	}
}

func TestInstalledCranePushesValidatedOCILayout(t *testing.T) {
	if _, err := exec.LookPath("crane"); err != nil {
		t.Skip("crane not installed; workflow must supply trusted uploader")
	}
	bundle, source, _ := preparedFixture(t)
	helperDir := t.TempDir()
	helperMarker := filepath.Join(helperDir, "helper-called")
	writeTool(t, helperDir, "docker-credential-gcloud", fmt.Sprintf("#!/bin/sh\n[ \"${GITHUB_TOKEN:-}\" = '' ] || exit 99\n[ \"$1\" = get ] || exit 98\ncat >/dev/null\necho called >> %q\necho '{\"Username\":\"fixture\",\"Secret\":\"not-a-real-token\"}'\n", helperMarker))
	t.Setenv("PATH", helperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GITHUB_TOKEN", "must-not-enter-credential-helper")
	var mu sync.Mutex
	blobs := map[string][]byte{}
	uploads := map[string][]byte{}
	manifests := map[string][]byte{}
	uploadID := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
		user, pass, ok := r.BasicAuth()
		if !ok || user != "fixture" || pass != "not-a-real-token" {
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
			w.WriteHeader(401)
			return
		}
		p := r.URL.Path
		switch {
		case p == "/v2/":
			w.WriteHeader(200)
		case strings.Contains(p, "/blobs/uploads/"):
			uploadID++
			upload := fmt.Sprintf("/v2/controller/blobs/uploads/%d", uploadID)
			switch r.Method {
			case http.MethodPost:
				uploads[upload] = nil
				w.Header().Set("Location", upload)
				w.WriteHeader(202)
			case http.MethodPatch:
				data, _ := io.ReadAll(r.Body)
				uploads[p] = append(uploads[p], data...)
				w.Header().Set("Location", p)
				w.WriteHeader(202)
			case http.MethodPut:
				data, _ := io.ReadAll(r.Body)
				complete := append(uploads[p], data...)
				h := sha256.Sum256(complete)
				digest := "sha256:" + hex.EncodeToString(h[:])
				if digest != r.URL.Query().Get("digest") {
					t.Error("uploaded blob does not match digest")
					w.WriteHeader(400)
					return
				}
				blobs[digest] = complete
				w.WriteHeader(201)
			default:
				w.WriteHeader(404)
			}
		case strings.Contains(p, "/blobs/"):
			digest := p[strings.LastIndex(p, "/")+1:]
			data, ok := blobs[digest]
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			if r.Method != http.MethodHead {
				_, _ = w.Write(data)
			}
		case strings.Contains(p, "/manifests/"):
			ref := p[strings.LastIndex(p, "/")+1:]
			if r.Method == http.MethodPut {
				data, _ := io.ReadAll(r.Body)
				var manifest struct {
					Config struct{ Digest string }
					Layers []struct{ Digest string }
				}
				if err := json.Unmarshal(data, &manifest); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				for _, digest := range append([]string{manifest.Config.Digest}, func() []string {
					var result []string
					for _, layer := range manifest.Layers {
						result = append(result, layer.Digest)
					}
					return result
				}()...) {
					if _, ok := blobs[digest]; !ok {
						t.Errorf("manifest references missing blob %s", digest)
						w.WriteHeader(400)
						return
					}
				}
				manifests[ref] = data
				h := sha256.Sum256(data)
				manifests["sha256:"+hex.EncodeToString(h[:])] = data
				w.WriteHeader(201)
				return
			}
			data, ok := manifests[ref]
			if !ok {
				w.WriteHeader(404)
				return
			}
			h := sha256.Sum256(data)
			w.Header().Set("Docker-Content-Digest", "sha256:"+hex.EncodeToString(h[:]))
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			if r.Method != http.MethodHead {
				_, _ = w.Write(data)
			}
		default:
			t.Errorf("unexpected registry route %s %s", r.Method, p)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	// Isolated empty registry config: never inspect or use personal Docker auth.
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	target := lifecycleTarget{Repository: strings.TrimPrefix(server.URL, "http://") + "/controller"}
	err := withOwnedLease(context.Background(), &memoryLeaseClient{}, "ci", false, func(ctx context.Context, o *leaseOwner) error {
		image, err := pushPreparedController(ctx, o, target, bundle, source)
		if err == nil && !strings.HasPrefix(image, target.Repository+"@sha256:") {
			t.Errorf("%q", image)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(helperMarker); err != nil {
		t.Fatal("registry did not use refreshable gcloud credential helper")
	}
}

func TestDrainRejectsUnattributedVMWithoutKarpenterName(t *testing.T) {
	target, _, client, dir := lifecycleFixture(t)
	if err := os.WriteFile(filepath.Join(dir, "instances.json"), []byte(`[{"name":"unattributed-legacy","id":"1","labels":{"goog-k8s-cluster-name":"test-cluster"}}]`), 0600); err != nil {
		t.Fatal(err)
	}
	err := withOwnedLease(context.Background(), client, "ci", true, func(ctx context.Context, o *leaseOwner) error { return drainOwned(ctx, o, target) })
	if err == nil || !strings.Contains(err.Error(), "maintenance-required") {
		t.Fatalf("unknown VM silently excluded: %v", err)
	}
}

func TestPreparedTargetRejectsTagResourceWhereTestsRequireShortName(t *testing.T) {
	target, _, _, _ := lifecycleFixture(t)
	target.TagKey = "tagKeys/123"
	setLifecycleTargetEnv(t, target)
	if _, err := loadLifecycleTarget(true); err == nil {
		t.Fatal("resource ID accepted as short tag key; networking would construct project/tagKeys/id")
	}
}

func TestPreparedLifecycleRejectsCredentialsInsideResults(t *testing.T) {
	target, input, client, dir := lifecycleFixture(t)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(input.OutputDir, "credentials.json"))
	if err := runPreparedLifecycle(context.Background(), client, "ci", target, input); err == nil {
		t.Fatal("result root could contain runtime credentials")
	}
	operations, _ := os.ReadFile(filepath.Join(dir, "operations"))
	if strings.Contains(string(operations), "push ") {
		t.Fatal("pushed before credential path isolation check")
	}
}

func TestLeaseReleaseFailureCannotLeaveSuccessfulOutcome(t *testing.T) {
	target, input, client, _ := lifecycleFixture(t)
	boundary := &releaseFailureBoundary{memoryLeaseClient: client}
	err := runPreparedLifecycle(context.Background(), boundary, "ci", target, input)
	outcome := readLifecycleOutcome(t, input.OutputDir)
	if err == nil || !errors.Is(err, errLeaseUncertain) || outcome.Tests != statePassed || outcome.Cleanup != stateIncomplete || outcome.Overall() == statePassed {
		t.Fatalf("%+v: %v", outcome, err)
	}
}

type releaseFailureBoundary struct{ *memoryLeaseClient }

func (c *releaseFailureBoundary) Update(ctx context.Context, l *coordinationv1.Lease, opts metav1.UpdateOptions) (*coordinationv1.Lease, error) {
	if l.Spec.HolderIdentity == nil {
		return nil, fmt.Errorf("release API unavailable")
	}
	return c.memoryLeaseClient.Update(ctx, l, opts)
}

func TestExplicitLocalBuildDeployRetainsKoModeWithoutNestedLock(t *testing.T) {
	target, _, client, dir := lifecycleFixture(t)
	setLifecycleTargetEnv(t, target)
	serveLeaseBoundary(t, client)
	t.Setenv("RELEASE_VERSION", "")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/not-read/adc.json")
	writeTool(t, dir, "helm", fmt.Sprintf("#!/bin/sh\nprintf 'helm %%s\\n' \"$*\" >> %q\n", filepath.Join(dir, "operations")))
	writeTool(t, dir, "gcloud", fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *configure-docker*) printf 'gcloud %%s\\n' \"$*\" >> %q;; *) exec %q \"$@\";;esac\n", filepath.Join(dir, "operations"), filepath.Join(dir, "gcloud-read")))
	// Preserve the preflight/cloud-read boundary alongside the local auth command.
	original := fmt.Sprintf("#!/bin/sh\nf=%q\ncase \"$*\" in 'container clusters describe '*) cat \"$f/cluster\";; 'compute instances list '*|'compute disks list '*) echo '[]';; *) exit 97;;esac\n", dir)
	writeTool(t, dir, "gcloud-read", original)
	seen := make(chan struct{}, 1)
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- struct{}{}
		if _, err := acquireLease(context.Background(), client, "competing-ci", false); err == nil {
			t.Error("local ko build was unlocked")
		}
	}))
	defer callback.Close()
	writeTool(t, dir, "ko", fmt.Sprintf("#!/bin/sh\nprintf 'ko %%s\\n' \"$*\" >> %q\ncurl -fsS %q >/dev/null\necho %q\n", filepath.Join(dir, "operations"), callback.URL, target.Repository+"@sha256:"+strings.Repeat("a", 64)))
	kubectl, err := os.ReadFile(filepath.Join(dir, "kubectl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(strings.Replace(string(kubectl), `"get priorityclass gmp-critical -o name")`, `"get priorityclass gmp-critical"|"get priorityclass gmp-critical -o name")`, 1)), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := newRunnerCommand()
	cmd.SetArgs([]string{"local-deploy", "--lock-id", "local-build"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-seen:
	default:
		t.Fatal("local ko not executed")
	}
	ops, _ := os.ReadFile(filepath.Join(dir, "operations"))
	if strings.Contains(string(ops), "controller.image.digest=") {
		t.Fatal("local deploy must preserve the e2e-SHA tag consumed by local test verification")
	}
	for _, arg := range []string{"helm dependency build ", "ko build --bare --platform linux/amd64", "controller.image.tag=e2e-"} {
		if !strings.Contains(string(ops), arg) {
			t.Fatalf("missing %q in %s", arg, ops)
		}
	}
}

func TestVerifiedGKEBootstrapVMsAndDisksAreNotCleanupCandidates(t *testing.T) {
	target, input, client, dir := lifecycleFixture(t)
	group := "https://www.googleapis.com/compute/v1/projects/fixture-pass/zones/us-central1-a/instanceGroupManagers/gke-system"
	disk := "https://www.googleapis.com/compute/v1/projects/fixture-pass/zones/us-central1-a/disks/gke-system"
	data, err := os.ReadFile(filepath.Join(dir, "cluster"))
	if err != nil {
		t.Fatal(err)
	}
	var cluster map[string]any
	if err := json.Unmarshal(data, &cluster); err != nil {
		t.Fatal(err)
	}
	cluster["nodePools"] = []any{map[string]any{"instanceGroupUrls": []string{group}}}
	files := map[string]any{"cluster": cluster, "instances.json": []any{map[string]any{"name": "gke-system", "id": "111", "selfLink": "https://www.googleapis.com/compute/v1/projects/fixture-pass/zones/us-central1-a/instances/gke-system", "labels": map[string]string{"goog-k8s-cluster-name": target.Cluster, "goog-k8s-cluster-location": target.Location}, "metadata": map[string]any{"items": []any{map[string]string{"key": "created-by", "value": group}}}, "disks": []any{map[string]string{"source": disk}}}}, "disks.json": []any{map[string]any{"name": "gke-system", "id": "222", "selfLink": disk}}}
	for name, value := range files {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := runPreparedLifecycle(context.Background(), client, "ci", target, input); err != nil {
		t.Fatal(err)
	}
	ops, _ := os.ReadFile(filepath.Join(dir, "operations"))
	if strings.Contains(string(ops), "delete instances") || strings.Contains(string(ops), "delete disks") {
		t.Fatal("cloud deletion bypassed controller")
	}
}

func TestMissingRuntimeBootstrapStopsBeforeRegistryPush(t *testing.T) {
	target, input, client, dir := lifecycleFixture(t)
	if err := os.WriteFile(filepath.Join(dir, "missing-priority"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := runPreparedLifecycle(context.Background(), client, "ci", target, input); err == nil {
		t.Fatal("missing bootstrap accepted")
	}
	ops, _ := os.ReadFile(filepath.Join(dir, "operations"))
	if strings.Contains(string(ops), "push ") {
		t.Fatal("pushed before checking bootstrap")
	}
}

func TestMaintenanceBlockIsDurableAndCannotRenderSuccess(t *testing.T) {
	target, input, client, _ := lifecycleFixture(t)
	client.lease.Annotations = map[string]string{maintenanceKey: "incomplete-drain"}
	if err := runPreparedLifecycle(context.Background(), client, "ci", target, input); err == nil {
		t.Fatal("blocked target accepted")
	}
	outcome := readLifecycleOutcome(t, input.OutputDir)
	if !outcome.MaintenanceRequired {
		t.Fatal("maintenance requirement not durable")
	}
	outcome.Infrastructure, outcome.Tests, outcome.Reports, outcome.Cleanup = statePassed, statePassed, statePassed, statePassed
	outcome.Counts = specCounts{Executed: 1, Passed: 1}
	if outcome.Overall() == statePassed {
		t.Fatal("maintenance-required outcome called passing")
	}
}

func TestTargetPreflightRejectsWrongEndpointAndFeatureRanges(t *testing.T) {
	for _, mode := range []string{"endpoint", "range-membership", "range-semantics", "tag-value"} {
		t.Run(mode, func(t *testing.T) {
			target, input, client, dir := lifecycleFixture(t)
			file := "cluster"
			needle, replacement := "127.0.0.1", "192.0.2.1"
			switch mode {
			case "range-membership":
				needle, replacement = `"small","large"`, `"small"`
			case "range-semantics":
				file = "subnet"
				needle, replacement = "10.2.0.0/20", "10.2.0.0/24"
			case "tag-value":
				file = "gcloud"
				needle, replacement = `"shortName":"e2e"`, `"shortName":"missing"`
			}
			data, err := os.ReadFile(filepath.Join(dir, file))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), needle) {
				t.Fatal("fixture replacement not found")
			}
			if err := os.WriteFile(filepath.Join(dir, file), []byte(strings.Replace(string(data), needle, replacement, 1)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := runPreparedLifecycle(context.Background(), client, "ci", target, input); err == nil {
				t.Fatalf("%s preflight accepted", mode)
			}
			ops, _ := os.ReadFile(filepath.Join(dir, "operations"))
			if strings.Contains(string(ops), "push ") || strings.Contains(string(ops), "delete ") {
				t.Fatal("preflight rejection mutated target")
			}
		})
	}
}

func TestCloudDeletionRejectsSameNameWithReplacementID(t *testing.T) {
	target, _, client, dir := lifecycleFixture(t)
	seedDrainCloud(t, dir, target)
	if err := os.WriteFile(filepath.Join(dir, "sticky-cloud"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "kubectl"))
	if err != nil {
		t.Fatal(err)
	}
	needle := `echo '{"items":[]}' > "$f/$2.json";`
	replacement := needle + `if [ "$2" = nodeclaims.karpenter.sh ];then sed 's/12345/99999/' "$f/instances.json" > "$f/replacement";mv "$f/replacement" "$f/instances.json";fi;`
	if !strings.Contains(string(data), needle) {
		t.Fatal("fixture replacement not found")
	}
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(strings.Replace(string(data), needle, replacement, 1)), 0700); err != nil {
		t.Fatal(err)
	}
	err = withOwnedLease(context.Background(), client, "ci", true, func(ctx context.Context, o *leaseOwner) error {
		cleanup, stop := o.cleanupContext(3 * time.Second)
		defer stop()
		return drainOwned(cleanup, o, target)
	})
	if err == nil || !strings.Contains(err.Error(), "identity replaced") {
		t.Fatalf("%v", err)
	}
}

func TestLeaseNamespaceCannotBeOverridden(t *testing.T) {
	client := &memoryLeaseClient{lease: &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: leaseName, ResourceVersion: "1"}}}
	serveLeaseBoundary(t, client)
	t.Setenv("KARPENTER_NAMESPACE", "different-controller-namespace")
	boundary, err := newLeaseClient()
	if err != nil {
		t.Fatal(err)
	}
	if err := withLease(context.Background(), boundary, "local", func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestDiagnosticsDoNotFollowPRCreatedSymlink(t *testing.T) {
	target, input, client, _ := lifecycleFixture(t)
	sentinel := filepath.Join(t.TempDir(), "outside-results")
	if err := os.WriteFile(sentinel, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := os.Symlink(sentinel, filepath.Join(input.OutputDir, "controller.log")); err != nil {
			t.Error(err)
		}
		cancel()
	}))
	defer callback.Close()
	target.TagKey = callback.URL
	_ = runPreparedLifecycle(ctx, client, "ci", target, input)
	data, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "unchanged" {
		t.Fatal("diagnostics followed PR-created symlink outside results")
	}
}

func TestPreparedLifecyclePreservesBootstrapNamespaceAndBinding(t *testing.T) {
	target, input, client, dir := lifecycleFixture(t)
	namespace := `{"metadata":{"name":"karpenter-e2e-test","uid":"bootstrap-namespace","labels":{"karpenter-e2e/owned":"true"}}}`
	binding := `{"metadata":{"name":"e2e-runtime-test-workloads","namespace":"karpenter-e2e-test","uid":"bootstrap-binding"},"roleRef":{"apiGroup":"rbac.authorization.k8s.io","kind":"ClusterRole","name":"e2e-runtime-test-workloads"},"subjects":[{"kind":"User","apiGroup":"rbac.authorization.k8s.io","name":"test-ci-sa@fixture-pass.iam.gserviceaccount.com"}]}`
	for name, content := range map[string]string{"namespace": namespace, "binding": binding} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := runPreparedLifecycle(context.Background(), client, "ci", target, input); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"namespace": namespace, "binding": binding} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(data) != want {
			t.Fatalf("bootstrap %s changed: %s %v", name, data, err)
		}
	}
	operations, _ := os.ReadFile(filepath.Join(dir, "operations"))
	for _, forbidden := range []string{"delete namespace", "create -f", "create rolebinding", "patch namespace", "label namespace"} {
		if strings.Contains(string(operations), forbidden) {
			t.Fatalf("bootstrap repair/deletion: %s", operations)
		}
	}
}

func TestMissingTestNamespaceBootstrapStopsBeforeRegistryPush(t *testing.T) {
	for _, missing := range []string{"namespace", "binding"} {
		t.Run(missing, func(t *testing.T) {
			target, input, client, dir := lifecycleFixture(t)
			if err := os.Remove(filepath.Join(dir, missing)); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			err := runPreparedLifecycle(context.Background(), client, "ci", target, input)
			operations, _ := os.ReadFile(filepath.Join(dir, "operations"))
			if err == nil || strings.Contains(string(operations), "push ") {
				t.Fatalf("missing %s reached upload: %v %s", missing, err, operations)
			}
		})
	}
}

func TestPreparedLifecycleDrainsOnlyProvenOwnedWorkloads(t *testing.T) {
	target, input, client, dir := lifecycleFixture(t)
	for file, content := range map[string]string{
		"deployments.apps.json": `{"items":[{"metadata":{"name":"owned","namespace":"karpenter-e2e-test","uid":"deployment-1","labels":{"karpenter-e2e/owned":"true"}}}]}`,
		"replicasets.apps.json": `{"items":[{"metadata":{"name":"owned-rs","namespace":"karpenter-e2e-test","uid":"rs-1","ownerReferences":[{"apiVersion":"apps/v1","kind":"Deployment","name":"owned","uid":"deployment-1","controller":true}]}}]}`,
		"pods.json":             `{"items":[{"metadata":{"name":"owned-pod","namespace":"karpenter-e2e-test","uid":"pod-1","ownerReferences":[{"apiVersion":"apps/v1","kind":"ReplicaSet","name":"owned-rs","uid":"rs-1","controller":true}]}}]}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := runPreparedLifecycle(context.Background(), client, "ci", target, input); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "deployments.apps.json"))
	if string(data) != "{\"items\":[]}\n" {
		t.Fatalf("owned Deployment survived: %s", data)
	}
	operations, _ := os.ReadFile(filepath.Join(dir, "operations"))
	if !strings.Contains(string(operations), "delete deployments.apps --field-selector=metadata.name=owned --selector=karpenter-e2e/owned=true --cascade=foreground") {
		t.Fatalf("missing bounded owned workload deletion: %s", operations)
	}
}

func TestUnknownTestWorkloadStopsBeforeAnyMutation(t *testing.T) {
	for _, tc := range []struct{ name, resource, object string }{
		{"unlabeled_deployment", "deployments.apps", `{"metadata":{"name":"legacy","namespace":"karpenter-e2e-test","uid":"legacy"}}`},
		{"owned_label_without_identity", "deployments.apps", `{"metadata":{"name":"legacy","namespace":"karpenter-e2e-test","labels":{"karpenter-e2e/owned":"true"}}}`},
		{"orphan_replicaset", "replicasets.apps", `{"metadata":{"name":"legacy-rs","namespace":"karpenter-e2e-test","uid":"legacy-rs"}}`},
		{"unproven_pod", "pods", `{"metadata":{"name":"legacy-pod","namespace":"karpenter-e2e-test","uid":"legacy-pod","labels":{"karpenter-e2e/owned":"true"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, input, client, dir := lifecycleFixture(t)
			if err := os.WriteFile(filepath.Join(dir, tc.resource+".json"), []byte(`{"items":[`+tc.object+`]}`), 0600); err != nil {
				t.Fatal(err)
			}
			err := runPreparedLifecycle(context.Background(), client, "ci", target, input)
			if err == nil || !strings.Contains(err.Error(), "unknown test workload") {
				t.Fatalf("unknown workload accepted: %v", err)
			}
			operations, _ := os.ReadFile(filepath.Join(dir, "operations"))
			for _, forbidden := range []string{"push ", "upgrade ", "delete ", "create "} {
				if strings.Contains(string(operations), forbidden) {
					t.Fatalf("mutated before proof: %s", operations)
				}
			}
			lease, _ := client.Get(context.Background(), leaseName, metav1.GetOptions{})
			if lease.Annotations[maintenanceKey] != "incomplete-drain" {
				t.Fatal("unknown workloads did not block next acquisition")
			}
		})
	}
}

func TestTestRoleBindingMismatchStopsBeforeRegistryPush(t *testing.T) {
	for _, replacement := range []string{"other-role", "other@fixture-pass.iam.gserviceaccount.com"} {
		t.Run(replacement, func(t *testing.T) {
			target, input, client, dir := lifecycleFixture(t)
			data, err := os.ReadFile(filepath.Join(dir, "binding"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(replacement, "@") {
				data = []byte(strings.ReplaceAll(string(data), "test-ci-sa@fixture-pass.iam.gserviceaccount.com", replacement))
			} else {
				data = []byte(strings.ReplaceAll(string(data), `"name":"e2e-runtime-test-workloads"}`, `"name":"`+replacement+`"}`))
			}
			if err := os.WriteFile(filepath.Join(dir, "binding"), data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := runPreparedLifecycle(context.Background(), client, "ci", target, input); err == nil || !strings.Contains(err.Error(), "bootstrap mismatch") {
				t.Fatalf("bad binding accepted: %v", err)
			}
			operations, _ := os.ReadFile(filepath.Join(dir, "operations"))
			if strings.Contains(string(operations), "push ") {
				t.Fatal("upload before bootstrap check")
			}
		})
	}
}

func TestWorkloadOwnerReferenceMustMatchCapturedUID(t *testing.T) {
	target, input, client, dir := lifecycleFixture(t)
	for name, data := range map[string]string{
		"deployments.apps.json": `{"items":[{"metadata":{"name":"owned","namespace":"karpenter-e2e-test","uid":"new-deployment","labels":{"karpenter-e2e/owned":"true"}}}]}`,
		"replicasets.apps.json": `{"items":[{"metadata":{"name":"old-rs","namespace":"karpenter-e2e-test","uid":"old-rs","ownerReferences":[{"apiVersion":"apps/v1","kind":"Deployment","name":"owned","uid":"old-deployment","controller":true}]}}]}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := runPreparedLifecycle(context.Background(), client, "ci", target, input); err == nil || !strings.Contains(err.Error(), "unknown test workload replicasets.apps") {
		t.Fatalf("owner-reference UID mismatch accepted: %v", err)
	}
	operations, _ := os.ReadFile(filepath.Join(dir, "operations"))
	if strings.Contains(string(operations), "delete ") {
		t.Fatal("deleted workload before validating descendant identity")
	}
}

func TestLocalCleanupDoesNotRequireOptInCIIdentityBootstrap(t *testing.T) {
	target, _, client, dir := lifecycleFixture(t)
	setLifecycleTargetEnv(t, target)
	serveLeaseBoundary(t, client)
	for _, name := range []string{"namespace", "binding"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	cmd := newRunnerCommand()
	cmd.SetArgs([]string{"local-clean-env", "--lock-id", "local-default-off"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("default-off manual target unexpectedly requires CI-only identity: %v", err)
	}
	operations, _ := os.ReadFile(filepath.Join(dir, "operations"))
	if strings.Contains(string(operations), "get rolebinding") || strings.Contains(string(operations), "delete namespace") || strings.Contains(string(operations), "create -f") {
		t.Fatalf("local cleanup touched CI bootstrap: %s", operations)
	}
}

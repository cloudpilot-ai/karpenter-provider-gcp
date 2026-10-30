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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/onsi/ginkgo/v2/types"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
)

func TestLocalSSDSelection(t *testing.T) {
	for _, tc := range []struct {
		selection string
		basic     bool
		extended  bool
	}{
		{selection: "local-ssd", basic: true},
		{selection: "local-ssd-extended", extended: true},
		{selection: "local-ssd,local-ssd-extended", basic: true, extended: true},
		{selection: "all", basic: true, extended: true},
	} {
		t.Run(tc.selection, func(t *testing.T) {
			filter, err := selectFilter("../../test/suites", tc.selection)
			if err != nil {
				t.Fatal(err)
			}
			matches, err := types.ParseLabelFilter(filter)
			if err != nil {
				t.Fatal(err)
			}
			for suite, want := range map[string]bool{
				"local-ssd":          tc.basic,
				"local-ssd-extended": tc.extended,
			} {
				if got := matches([]string{"suite:" + suite}); got != want {
					t.Fatalf("%s selection matches %s = %v, want %v", tc.selection, suite, got, want)
				}
			}
		})
	}
}

func TestStandardSelection(t *testing.T) {
	filter, err := selectFilter("../../test/suites", "standard")
	if err != nil {
		t.Fatal(err)
	}
	matches, err := types.ParseLabelFilter(filter)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		suite string
		want  bool
	}{
		{suite: "gpu", want: false},
		{suite: "local-ssd-extended", want: false},
		{suite: "local-ssd", want: true},
		{suite: "storage", want: true},
		{suite: "provisioning", want: true},
	} {
		t.Run(tc.suite, func(t *testing.T) {
			if got := matches([]string{"suite:" + tc.suite}); got != tc.want {
				t.Fatalf("standard selection matches %s = %v, want %v", tc.suite, got, tc.want)
			}
		})
	}
}

func TestLocalCLIReportRetainsNativeAndFailsOnEmpty(t *testing.T) {
	dir := t.TempDir()
	writeNativeFixture(t, dir, types.Report{SuiteSucceeded: true, SpecReports: types.SpecReports{{LeafNodeType: types.NodeTypeIt, State: types.SpecStatePassed, LeafNodeText: "local passed", LeafNodeLabels: []string{"suite:storage"}}}})
	var mu sync.Mutex
	lease := coordinationv1.Lease{TypeMeta: metav1.TypeMeta{APIVersion: "coordination.k8s.io/v1", Kind: "Lease"}, ObjectMeta: metav1.ObjectMeta{Name: leaseName, ResourceVersion: "1"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPut {
			data, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				t.Error(readErr)
				w.WriteHeader(400)
				return
			}
			if _, _, err := scheme.Codecs.UniversalDeserializer().Decode(data, nil, &lease); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
		}
		if err := json.NewEncoder(w).Encode(lease); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	config := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(config, []byte(fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: fixture\n  cluster:\n    server: %s\ncontexts:\n- name: fixture\n  context:\n    cluster: fixture\ncurrent-context: fixture\n", server.URL)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", config)
	t.Setenv("PROJECT_ID", "")
	t.Setenv("CLUSTER_LOCATION", "")
	t.Setenv("CLUSTER_NAME", "")
	scripts := map[string]string{
		"git":     "#!/bin/sh\ncase \"$*\" in *rev-parse*) echo abc1234;; *status*) exit 0;; *) exit 97;;esac\n",
		"kubectl": "#!/bin/sh\ncase \"$*\" in *'get deployment'*) echo example.invalid/controller:e2e-abc1234;; *logs*) echo '{\"commit\":\"abc1234\"}';; *) exit 97;;esac\n",
		"go": `#!/bin/sh
empty=no
junit=no
for arg in "$@";do case "$arg" in
 --output-dir=*) out=${arg#*=};;
 --fail-on-empty) empty=yes;;
 --junit-report=junit.xml) junit=yes;;
esac;done
[ "$empty" = yes ] || exit 96
[ "$junit" = yes ] || exit 95
cp "$LOCAL_NATIVE_FIXTURE/results.json" "$out/results.json"
cp "$LOCAL_NATIVE_FIXTURE/junit.xml" "$out/junit.xml"
`,
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("LOCAL_NATIVE_FIXTURE", dir)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, flag := range []string{"--selection", "--preset"} {
		report := filepath.Join(dir, strings.TrimPrefix(flag, "--")+".md")
		command := newRunnerCommand()
		command.SetArgs([]string{flag, "standard", "--lock-id", "local-fixture", "--report", report})
		if err := command.Execute(); err != nil {
			t.Fatal(err)
		}
		rendered, err := os.ReadFile(report)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(rendered), "abc1234") || !strings.Contains(string(rendered), "local passed") {
			t.Fatalf("invalid local report: %s", rendered)
		}
		matches, err := filepath.Glob(report + "-native-*")
		if err != nil || len(matches) != 1 {
			t.Fatalf("native directory not retained: %v %v", matches, err)
		}
		for _, name := range []string{"results.json", "junit.xml"} {
			if _, err := os.Stat(filepath.Join(matches[0], name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if lease.Spec.HolderIdentity != nil {
		t.Fatal("local CLI did not release lease")
	}
}

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

package e2eidentity_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
)

type bootstrapObject struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Rules             []rbacv1.PolicyRule `json:"rules"`
	RoleRef           rbacv1.RoleRef      `json:"roleRef"`
	Subjects          []rbacv1.Subject    `json:"subjects"`
}

func readBootstrap(t *testing.T) []bootstrapObject {
	t.Helper()
	data, err := os.ReadFile("ci-bootstrap-rbac.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return decodeRBAC(t, data)
}

func decodeRBAC(t *testing.T, data []byte) []bootstrapObject {
	t.Helper()
	decoder := kyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var objects []bootstrapObject
	for {
		var object bootstrapObject
		err := decoder.Decode(&object)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		objects = append(objects, object)
	}
	return objects
}

// Static RBAC audit only. Live GKE authentication, admission and inherited
// authorization still require separate activation proof.
func bootstrapAllows(objects []bootstrapObject, namespace, verb, group, resource, name string) bool {
	for _, binding := range objects {
		if !bindingApplies(binding, namespace) {
			continue
		}
		for _, role := range objects {
			if role.Kind != binding.RoleRef.Kind || role.Name != binding.RoleRef.Name || role.Kind == "Role" && role.Namespace != binding.Namespace {
				continue
			}
			for _, rule := range role.Rules {
				if ruleAllows(rule, verb, group, resource, name) {
					return true
				}
			}
		}
	}
	return false
}

func bindingApplies(binding bootstrapObject, namespace string) bool {
	scoped := binding.Kind == "ClusterRoleBinding" || binding.Kind == "RoleBinding" && binding.Namespace == namespace
	return scoped && len(binding.Subjects) == 1 && binding.Subjects[0] == (rbacv1.Subject{Kind: "User", APIGroup: rbacv1.GroupName, Name: "CI_RUNTIME_SERVICE_ACCOUNT_EMAIL"})
}

func ruleAllows(rule rbacv1.PolicyRule, verb, group, resource, name string) bool {
	return slices.Contains(rule.Verbs, verb) && slices.Contains(rule.APIGroups, group) && slices.Contains(rule.Resources, resource) && (len(rule.ResourceNames) == 0 || slices.Contains(rule.ResourceNames, name))
}

func TestBootstrapConfinesWorkloadWritesWithoutEscalation(t *testing.T) {
	objects := readBootstrap(t)
	for _, object := range objects {
		for _, rule := range object.Rules {
			for _, value := range append(append(slices.Clone(rule.Verbs), rule.APIGroups...), rule.Resources...) {
				if value == "*" {
					t.Fatalf("wildcard in %s/%s", object.Kind, object.Name)
				}
			}
			for _, verb := range []string{"bind", "escalate", "impersonate"} {
				if slices.Contains(rule.Verbs, verb) {
					t.Fatalf("%s bypass in %s", verb, object.Name)
				}
			}
		}
	}
	for _, tc := range []struct {
		namespace, verb, group, resource, name string
		allowed                                bool
	}{
		{"karpenter-e2e-test", "create", "apps", "deployments", "fixture", true},
		{"karpenter-e2e-test", "delete", "apps", "deployments", "fixture", true},
		{"other", "create", "apps", "deployments", "fixture", false},
		{"other", "delete", "apps", "deployments", "fixture", false},
		{"karpenter-system", "create", "", "secrets", "sh.helm.release.v1.karpenter.v1", true},
		{"other", "get", "", "secrets", "unrelated", false},
		{"", "create", "", "namespaces", "karpenter-e2e-test", false},
		{"", "delete", "", "namespaces", "karpenter-e2e-test", false},
		{"karpenter-e2e-test", "patch", "rbac.authorization.k8s.io", "rolebindings", "e2e-runtime-test-workloads", false},
		{"", "escalate", "rbac.authorization.k8s.io", "clusterroles", "karpenter", false},
		{"", "patch", "rbac.authorization.k8s.io", "clusterroles", "e2e-runtime-cluster", false},
		{"", "patch", "apiextensions.k8s.io", "customresourcedefinitions", "unrelated.example", false},
		{"karpenter-system", "get", "coordination.k8s.io", "leases", "karpenter-e2e-tests", true},
		{"karpenter-system", "update", "coordination.k8s.io", "leases", "karpenter-e2e-tests", true},
	} {
		if got := bootstrapAllows(objects, tc.namespace, tc.verb, tc.group, tc.resource, tc.name); got != tc.allowed {
			t.Errorf("namespace=%q %s %s/%s name=%s allowed=%v want=%v", tc.namespace, tc.verb, tc.group, tc.resource, tc.name, got, tc.allowed)
		}
	}
}

func renderCurrentChartRBAC(t *testing.T) []bootstrapObject {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm unavailable; source RBAC rendering requires installed Helm")
	}
	chart := t.TempDir()
	if err := os.Mkdir(filepath.Join(chart, "templates"), 0700); err != nil {
		t.Fatal(err)
	}
	// Only render real RBAC templates/values: the unrelated optional subchart
	// needs no dependency download for this cloud-free permission audit.
	if err := os.WriteFile(filepath.Join(chart, "Chart.yaml"), []byte("apiVersion: v2\nname: karpenter\nversion: 0.0.0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"values.yaml", "templates/_helpers.tpl", "templates/clusterrole.yaml", "templates/clusterrole-core.yaml", "templates/role.yaml"} {
		data, err := os.ReadFile(filepath.Join("../../charts/karpenter", name))
		if err != nil {
			t.Fatal(err)
		}
		// #nosec G703 -- name is from the fixed literal list above; chart is t.TempDir.
		if err := os.WriteFile(filepath.Join(chart, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	data, err := exec.Command(helm, "template", "karpenter", chart, "--namespace", "karpenter-system").CombinedOutput()
	if err != nil {
		t.Fatalf("render actual chart RBAC: %s %v", data, err)
	}
	return decodeRBAC(t, data)
}

func TestBootstrapCoversCurrentHelmRBACWithoutEscalation(t *testing.T) {
	objects := readBootstrap(t)
	roles := 0
	for _, role := range renderCurrentChartRBAC(t) {
		if role.Kind != "ClusterRole" && role.Kind != "Role" {
			continue
		}
		roles++
		namespace := role.Namespace
		if role.Kind == "ClusterRole" {
			namespace = "unrelated-namespace"
		}
		for _, rule := range role.Rules {
			checkBootstrapCoversRule(t, objects, role, namespace, rule)
		}
	}
	if roles != 4 {
		t.Fatalf("expected both chart ClusterRoles and both namespace Roles; got %d", roles)
	}
}

func checkBootstrapCoversRule(t *testing.T, objects []bootstrapObject, role bootstrapObject, namespace string, rule rbacv1.PolicyRule) {
	t.Helper()
	names := rule.ResourceNames
	if len(names) == 0 {
		names = []string{""}
	}
	for _, verb := range rule.Verbs {
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				for _, name := range names {
					if !bootstrapAllows(objects, namespace, verb, group, resource, name) {
						t.Errorf("current %s/%s requires unavailable %s %s/%s name=%q namespace=%q", role.Kind, role.Name, verb, group, resource, name, namespace)
					}
				}
			}
		}
	}
}

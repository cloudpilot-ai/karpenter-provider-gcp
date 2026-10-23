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
	"os"
	"slices"
	"testing"

	"github.com/google/cel-go/cel"
	"sigs.k8s.io/yaml"
)

// This evaluates the shipped CEL, not a parallel Go authorization policy.
// Issuer, audience, signature and token exchange remain provider/live gates.
func TestCIOIDCCondition(t *testing.T) {
	condition, err := os.ReadFile("ci-oidc-condition.cel")
	if err != nil {
		t.Fatal(err)
	}
	env, err := cel.NewEnv(cel.Variable("assertion", cel.MapType(cel.StringType, cel.DynType)))
	if err != nil {
		t.Fatal(err)
	}
	ast, issues := env.Compile(string(condition))
	if issues.Err() != nil {
		t.Fatal(issues.Err())
	}
	program, err := env.Program(ast)
	if err != nil {
		t.Fatal(err)
	}
	valid := map[string]any{
		"repository_id": "863038341", "repository_owner_id": "166281999",
		"ref": "refs/heads/main", "event_name": "workflow_dispatch",
		"workflow_ref": "cloudpilot-ai/karpenter-provider-gcp/.github/workflows/e2e.yaml@refs/heads/main",
		"environment":  "e2e-runtime", "sub": "repo:cloudpilot-ai/karpenter-provider-gcp:environment:e2e-runtime",
	}
	evaluate := func(t *testing.T, claims map[string]any, want bool) {
		t.Helper()
		value, _, evalErr := program.Eval(map[string]any{"assertion": claims})
		allowed := evalErr == nil && value.Value() == true
		if allowed != want {
			t.Fatalf("allowed=%v, want=%v (CEL error: %v)", allowed, want, evalErr)
		}
	}
	t.Run("upstream_manual_main_environment_without_reusable_claim", func(t *testing.T) { evaluate(t, valid, true) })
	for claim, wrong := range map[string]any{
		"repository_id": "1", "repository_owner_id": "1", "ref": "refs/heads/feature",
		"workflow_ref": "cloudpilot-ai/karpenter-provider-gcp/.github/workflows/other.yaml@refs/heads/main",
		"event_name":   "pull_request_target", "environment": "other", "sub": "repo:fork/provider:environment:e2e-runtime",
	} {
		t.Run("wrong_"+claim, func(t *testing.T) {
			claims := cloneClaims(valid)
			claims[claim] = wrong
			evaluate(t, claims, false)
		})
		t.Run("missing_"+claim, func(t *testing.T) {
			claims := cloneClaims(valid)
			delete(claims, claim)
			evaluate(t, claims, false)
		})
	}
	for name, changes := range map[string]map[string]any{
		"fork_origin_workflow":   {"repository_id": "123", "repository_owner_id": "456", "workflow_ref": "fork/provider/.github/workflows/e2e.yaml@refs/heads/main", "sub": "repo:fork/provider:environment:e2e-runtime"},
		"same_repository_tag":    {"ref": "refs/tags/main"},
		"same_path_wrong_ref":    {"workflow_ref": "cloudpilot-ai/karpenter-provider-gcp/.github/workflows/e2e.yaml@refs/heads/feature"},
		"no_environment_subject": {"sub": "repo:cloudpilot-ai/karpenter-provider-gcp:ref:refs/heads/main"},
		"pull_request_event":     {"event_name": "pull_request"},
	} {
		t.Run(name, func(t *testing.T) {
			claims := cloneClaims(valid)
			for key, value := range changes {
				claims[key] = value
			}
			evaluate(t, claims, false)
		})
	}
}

func cloneClaims(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func TestCIRuntimePermissions(t *testing.T) {
	data, err := os.ReadFile("ci-runtime-role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var role struct {
		Title               string   `json:"title"`
		Description         string   `json:"description"`
		Stage               string   `json:"stage"`
		IncludedPermissions []string `json:"includedPermissions"`
	}
	if err := yaml.UnmarshalStrict(data, &role); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"compute.instances.get", "compute.instances.list", "compute.instances.listEffectiveTags",
		"compute.disks.get", "compute.disks.list", "compute.subnetworks.get",
		"container.clusters.get", "container.getServerConfig",
	}
	slices.Sort(role.IncludedPermissions)
	slices.Sort(want)
	if !slices.Equal(role.IncludedPermissions, want) {
		t.Fatalf("runtime permissions=%v, want only inventoried reads=%v", role.IncludedPermissions, want)
	}
}

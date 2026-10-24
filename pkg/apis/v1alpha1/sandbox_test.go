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

package v1alpha1

import (
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/require"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

func TestSandboxValidationAndHash(t *testing.T) {
	ordinary := &GCENodeClass{Spec: GCENodeClassSpec{ImageSelectorTerms: []ImageSelectorTerm{{Family: ImageFamilyContainerOptimizedOS, Channel: ImageChannelCluster}}}}
	require.NoError(t, ordinary.ValidateSandbox())
	sandbox := ordinary.DeepCopy()
	sandbox.Spec.SandboxConfig = &SandboxConfig{Type: SandboxTypeGVISOR}
	require.NoError(t, sandbox.ValidateSandbox())
	require.True(t, sandbox.SandboxEnabled())
	require.NotEqual(t, ordinary.Hash(), sandbox.Hash())
	copied := sandbox.DeepCopy()
	copied.Spec.SandboxConfig.Type = "INVALID"
	require.Equal(t, SandboxTypeGVISOR, sandbox.Spec.SandboxConfig.Type)
	require.Error(t, copied.ValidateSandbox())
	sandbox.Spec.SandboxConfig = nil
	require.Equal(t, ordinary.Hash(), sandbox.Hash())
	require.True(t, karpv1.WellKnownLabels.Has(LabelSandboxRuntime))

	for _, term := range []ImageSelectorTerm{{Family: ImageFamilyUbuntu2404, Version: "latest"}, {Alias: "Ubuntu@latest"}, {Alias: "Ubuntu2204@latest"}} {
		nc := ordinary.DeepCopy()
		nc.Spec.SandboxConfig = &SandboxConfig{Type: SandboxTypeGVISOR}
		nc.Spec.ImageSelectorTerms = append(nc.Spec.ImageSelectorTerms, term)
		require.Error(t, nc.ValidateSandbox())
	}
	for _, override := range []string{ImageFamilyUbuntu, ImageFamilyContainerOptimizedOS} {
		nc := ordinary.DeepCopy()
		nc.Spec.SandboxConfig = &SandboxConfig{Type: SandboxTypeGVISOR}
		nc.Spec.ImageFamily = &override
		if override == ImageFamilyUbuntu {
			require.Error(t, nc.ValidateSandbox())
		} else {
			require.NoError(t, nc.ValidateSandbox())
		}
	}
	nc := ordinary.DeepCopy()
	nc.Spec.SandboxConfig = &SandboxConfig{Type: SandboxTypeGVISOR}
	nc.Spec.GPUSharingConfig = &GPUSharingConfig{Strategy: GPUSharingStrategyTimeSharing, MaxSharedClientsPerGPU: 2}
	require.Error(t, nc.ValidateSandbox())
}

func TestSandboxCRDSchemaAndCEL(t *testing.T) {
	schema := specSchema(t)
	config := schema.Properties["sandboxConfig"]
	require.NotContains(t, schema.Required, "sandboxConfig")
	require.Equal(t, []string{"type"}, config.Required)
	require.Len(t, config.Properties["type"].Enum, 1)
	require.Equal(t, `"GVISOR"`, string(config.Properties["type"].Enum[0].Raw))
	env, err := cel.NewEnv(cel.Variable("self", cel.DynType))
	require.NoError(t, err)
	var programs []cel.Program
	for _, rule := range schema.XValidations {
		if !strings.HasPrefix(rule.Message, "GVISOR") {
			continue
		}
		ast, issues := env.Compile(rule.Rule)
		require.NoError(t, issues.Err())
		program, err := env.Program(ast)
		require.NoError(t, err)
		programs = append(programs, program)
	}
	require.Len(t, programs, 2)
	for _, tt := range []struct {
		name  string
		spec  map[string]any
		valid bool
	}{
		{"ordinary Ubuntu", map[string]any{"imageSelectorTerms": []any{map[string]any{"alias": "Ubuntu@latest"}}}, true},
		{"COS", map[string]any{"sandboxConfig": map[string]any{"type": "GVISOR"}, "imageSelectorTerms": []any{map[string]any{"family": "ContainerOptimizedOS", "channel": "cluster"}}}, true},
		{"public ID deferred to resolver", map[string]any{"sandboxConfig": map[string]any{"type": "GVISOR"}, "imageSelectorTerms": []any{map[string]any{"id": "projects/gke-node-images/global/images/image"}}}, true},
		{"mixed terms", map[string]any{"sandboxConfig": map[string]any{"type": "GVISOR"}, "imageSelectorTerms": []any{map[string]any{"family": "ContainerOptimizedOS"}, map[string]any{"family": "Ubuntu2404"}}}, false},
		{"legacy Ubuntu", map[string]any{"sandboxConfig": map[string]any{"type": "GVISOR"}, "imageSelectorTerms": []any{map[string]any{"alias": "Ubuntu@latest"}}}, false},
		{"contradictory override", map[string]any{"sandboxConfig": map[string]any{"type": "GVISOR"}, "imageFamily": "Ubuntu", "imageSelectorTerms": []any{map[string]any{"family": "ContainerOptimizedOS"}}}, false},
		{"time-sharing", map[string]any{"sandboxConfig": map[string]any{"type": "GVISOR"}, "gpuSharingConfig": map[string]any{"strategy": "TimeSharing", "maxSharedClientsPerGPU": 2}, "imageSelectorTerms": []any{map[string]any{"family": "ContainerOptimizedOS"}}}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			valid := true
			for _, program := range programs {
				result, _, err := program.Eval(map[string]any{"self": tt.spec})
				require.NoError(t, err)
				valid = valid && result.Value() == true
			}
			require.Equal(t, tt.valid, valid)
		})
	}
}

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

package crdtools_test

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

func TestGCENodeClassCRDValidatesShutdownDurations(t *testing.T) {
	raw, err := os.ReadFile("../../charts/karpenter/crds/karpenter.k8s.gcp_gcenodeclasses.yaml")
	require.NoError(t, err)
	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal(raw, &crd))
	require.Len(t, crd.Spec.Versions, 1)
	kubeletSchema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["kubeletConfiguration"]
	var internalSchema apiextensions.JSONSchemaProps
	require.NoError(t, apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&kubeletSchema, &internalSchema, nil))
	structural, err := schema.NewStructural(&internalSchema)
	require.NoError(t, err)
	openAPIValidator, _, err := validation.NewSchemaValidator(&internalSchema)
	require.NoError(t, err)
	validator := cel.NewValidator(structural, false, celconfig.PerCallLimit)
	require.NotNil(t, validator)

	for _, tc := range []struct {
		name      string
		config    map[string]interface{}
		wantValid bool
	}{
		{name: "omitted", config: map[string]interface{}{}, wantValid: true},
		{name: "standalone zero", config: map[string]interface{}{"shutdownGracePeriod": "0s"}, wantValid: true},
		{name: "standalone duration", config: map[string]interface{}{"shutdownGracePeriod": "30s"}, wantValid: true},
		{name: "maximum Go duration", config: map[string]interface{}{"shutdownGracePeriod": "9223372036854775807ns"}, wantValid: true},
		{name: "standalone overflow", config: map[string]interface{}{"shutdownGracePeriod": "2562048h"}},
		{name: "standalone malformed", config: map[string]interface{}{"shutdownGracePeriod": "30seconds"}},
		{name: "standalone negative", config: map[string]interface{}{"shutdownGracePeriod": "-5s"}},
		{name: "mixed units", config: map[string]interface{}{"shutdownGracePeriod": "2m", "shutdownGracePeriodCriticalPods": "90s"}, wantValid: true},
		{name: "critical pods only", config: map[string]interface{}{"shutdownGracePeriodCriticalPods": "15s"}},
		{name: "critical pods exceed total", config: map[string]interface{}{"shutdownGracePeriod": "30s", "shutdownGracePeriodCriticalPods": "1m"}},
		{name: "critical pods overflow", config: map[string]interface{}{"shutdownGracePeriod": "30s", "shutdownGracePeriodCriticalPods": "2562048h"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := field.NewPath("kubeletConfiguration")
			errs := validation.ValidateCustomResource(path, tc.config, openAPIValidator)
			celErrs, _ := validator.Validate(context.Background(), field.NewPath("kubeletConfiguration"), structural, tc.config, nil, celconfig.RuntimeCELCostBudget)
			errs = append(errs, celErrs...)
			if tc.wantValid {
				require.Empty(t, errs)
			} else {
				require.NotEmpty(t, errs)
			}
		})
	}
}

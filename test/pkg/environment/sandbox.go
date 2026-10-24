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

package environment

import (
	"context"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// CreateSandboxNodeClass creates an owned COS sandbox class without a native sandbox pool.
func (e *Environment) CreateSandboxNodeClass(ctx context.Context, name string, diskGiB int64) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "karpenter.k8s.gcp/v1alpha1",
		"kind":       "GCENodeClass",
		"metadata":   map[string]any{"name": name, "labels": map[string]any{e2eOwnerLabel: "true"}},
		"spec": map[string]any{
			"sandboxConfig":      map[string]any{"type": "GVISOR"},
			"imageSelectorTerms": []any{map[string]any{"family": "ContainerOptimizedOS", "channel": "cluster"}},
			"gpuDriverVersion":   "default",
			"disks":              []any{map[string]any{"boot": true, "sizeGiB": diskGiB, "category": "pd-balanced"}},
		},
	}}
	_, err := e.DynamicClient.Resource(gceNodeClassGVR).Create(ctx, obj, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred(), "creating sandbox GCENodeClass %s; upgrade controller/CRDs and review sandbox prerequisites", name)
	e.trackNodeClass(name)
}

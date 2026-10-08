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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"

	. "github.com/onsi/gomega"
	"golang.org/x/oauth2/google"
	compute "google.golang.org/api/compute/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ResourceManagerTagValue is the short name of the tag value e2e-setup creates under
// ResourceManagerTagKey. Must match hack/e2e-setup.sh.
const ResourceManagerTagValue = "e2e"

// CreateNodeClassWithResourceManagerTags creates a GCENodeClass that binds the given
// resource manager tags to provisioned instances.
func (e *Environment) CreateNodeClassWithResourceManagerTags(ctx context.Context, name string, tags map[string]string) {
	deleteIfExists(ctx, e.DynamicClient, gceNodeClassGVR, name)
	rmTags := make(map[string]any, len(tags))
	for k, v := range tags {
		rmTags[k] = v
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "karpenter.k8s.gcp/v1alpha1",
		"kind":       "GCENodeClass",
		"metadata":   map[string]any{"name": name, "labels": map[string]any{e2eOwnerLabel: "true"}},
		"spec": map[string]any{
			"imageSelectorTerms": []any{
				map[string]any{"alias": "ContainerOptimizedOS@latest"},
			},
			"disks": []any{
				map[string]any{"sizeGiB": int64(DefaultE2EDiskGiB), "boot": true},
			},
			"subnetRangeName":     e.PodsRangeName,
			"resourceManagerTags": rmTags,
		},
	}}
	_, err := e.DynamicClient.Resource(gceNodeClassGVR).Create(ctx, obj, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred(), "creating GCENodeClass %s", name)
	e.trackNodeClass(name)
}

type effectiveTag struct {
	NamespacedTagKey   string `json:"namespacedTagKey"`
	NamespacedTagValue string `json:"namespacedTagValue"`
}

// GetInstanceEffectiveTags returns the namespaced tag values bound to the instance, keyed by
// namespaced tag key. It reads Resource Manager directly because Compute treats
// resourceManagerTags as input-only and never returns them on the instance.
func (e *Environment) GetInstanceEffectiveTags(ctx context.Context, instance *compute.Instance) (map[string]string, error) {
	zone := path.Base(instance.Zone)
	parent := fmt.Sprintf("//compute.googleapis.com/projects/%s/zones/%s/instances/%d", e.ProjectID, zone, instance.Id)
	// Zonal resources are only visible through the location-specific Resource Manager endpoint.
	endpoint := fmt.Sprintf("https://%s-cloudresourcemanager.googleapis.com/v3/effectiveTags?parent=%s", zone, url.QueryEscape(parent))

	client, err := google.DefaultClient(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return nil, fmt.Errorf("creating Resource Manager client: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("listing effective tags for %s: %w", parent, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing effective tags for %s: HTTP %d: %s", parent, resp.StatusCode, body)
	}

	var out struct {
		EffectiveTags []effectiveTag `json:"effectiveTags"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decoding effective tags for %s: %w", parent, err)
	}
	tags := make(map[string]string, len(out.EffectiveTags))
	for _, t := range out.EffectiveTags {
		tags[t.NamespacedTagKey] = t.NamespacedTagValue
	}
	return tags, nil
}

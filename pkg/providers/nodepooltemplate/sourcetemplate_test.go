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

package nodepooltemplate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/container/v1"
)

const (
	testProject = "proj"
	testRegion  = "us-central1"
	testZone    = "us-central1-a"
	testCluster = "cluster"
	testPool    = "default-pool"
)

func migURL(zone, name string) string {
	return fmt.Sprintf("https://www.googleapis.com/compute/v1/projects/%s/zones/%s/instanceGroupManagers/%s",
		testProject, zone, name)
}

func regionalTemplateURL(name string) string {
	return fmt.Sprintf("https://www.googleapis.com/compute/v1/projects/%s/regions/%s/instanceTemplates/%s",
		testProject, testRegion, name)
}

func globalTemplateURL(name string) string {
	return fmt.Sprintf("https://www.googleapis.com/compute/v1/projects/%s/global/instanceTemplates/%s",
		testProject, name)
}

func instanceTemplate(name, clusterName, nodePoolName, creationTimestamp string) *compute.InstanceTemplate {
	return &compute.InstanceTemplate{
		Name:              name,
		CreationTimestamp: creationTimestamp,
		Properties: &compute.InstanceProperties{
			Labels:   map[string]string{nodePoolNameLabelKey: nodePoolName},
			Metadata: &compute.Metadata{Items: []*compute.MetadataItems{{Key: clusterNameMetadataKey, Value: lo.ToPtr(clusterName)}}},
		},
	}
}

// sourceTemplateFake serves the GKE and Compute endpoints GetSourceTemplateMetadata walks.
type sourceTemplateFake struct {
	// nodePool is returned by container NodePools.Get. nil → 404.
	nodePool *container.NodePool
	// managers maps an instance group manager name to the template URL it is running.
	managers map[string]string
	// managerStatus, when non-zero, is returned instead of serving managers.
	managerStatus int
	// templates are served by name from both the regional and global Get endpoints, and are
	// returned by the regional List endpoint one item per page.
	templates []*compute.InstanceTemplate
	// listOnly restricts the regional List endpoint to these templates. nil → all templates.
	listOnly []*compute.InstanceTemplate
}

func (f *sourceTemplateFake) templateByName(name string) *compute.InstanceTemplate {
	for _, template := range f.templates {
		if template.Name == name {
			return template
		}
	}
	return nil
}

func (f *sourceTemplateFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		name := path[strings.LastIndex(path, "/")+1:]

		switch {
		case strings.Contains(path, "/nodePools/"):
			if f.nodePool == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(f.nodePool)

		case strings.Contains(path, "/instanceGroupManagers/"):
			if f.managerStatus != 0 {
				w.WriteHeader(f.managerStatus)
				return
			}
			templateURL, ok := f.managers[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(&compute.InstanceGroupManager{Name: name, InstanceTemplate: templateURL})

		// The List endpoint has no trailing name, so it must be matched before the Get endpoints.
		case strings.HasSuffix(path, fmt.Sprintf("/regions/%s/instanceTemplates", testRegion)):
			f.serveList(w, r)

		case strings.Contains(path, "/instanceTemplates/"):
			template := f.templateByName(name)
			if template == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(template)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// serveList pages the regional instance template list one item at a time so that pagination is
// exercised whenever more than one template is listed.
func (f *sourceTemplateFake) serveList(w http.ResponseWriter, r *http.Request) {
	items := f.listOnly
	if items == nil {
		items = f.templates
	}
	page := 0
	if token := r.URL.Query().Get("pageToken"); token != "" {
		_, _ = fmt.Sscanf(token, "page-%d", &page)
	}
	if page >= len(items) {
		_ = json.NewEncoder(w).Encode(&compute.InstanceTemplateList{})
		return
	}
	resp := &compute.InstanceTemplateList{Items: []*compute.InstanceTemplate{items[page]}}
	if page+1 < len(items) {
		resp.NextPageToken = fmt.Sprintf("page-%d", page+1)
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (f *sourceTemplateFake) provider(t *testing.T) *DefaultProvider {
	t.Helper()
	srv := f.server(t)
	t.Cleanup(srv.Close)
	return &DefaultProvider{
		computeService:   buildComputeService(t, srv),
		containerService: buildContainerService(t, srv),
		sourcePoolName:   testPool,
		ClusterInfo: ClusterInfo{
			ProjectID:    testProject,
			Region:       testRegion,
			NodeLocation: testRegion,
			Name:         testCluster,
		},
	}
}

// templateName reads back the template the provider resolved, which the metadata alone cannot
// identify since every rotation of a pool's template carries identical cluster-name metadata.
func templateName(p *DefaultProvider) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastTemplateName
}

func TestGetSourceTemplateMetadata(t *testing.T) {
	t.Parallel()

	current := instanceTemplate("gke-current", testCluster, testPool, "2026-08-20T10:00:00.000-07:00")
	stale := instanceTemplate("gke-stale", testCluster, testPool, "2026-01-02T10:00:00.000-07:00")

	t.Run("resolves the regional template the node pool's instance group is running", func(t *testing.T) {
		t.Parallel()
		fake := &sourceTemplateFake{
			nodePool:  &container.NodePool{Name: testPool, InstanceGroupUrls: []string{migURL(testZone, "grp-a")}},
			managers:  map[string]string{"grp-a": regionalTemplateURL("gke-current")},
			templates: []*compute.InstanceTemplate{stale, current},
			// The pool names the current template, so the label scan must not be consulted at all.
			listOnly: []*compute.InstanceTemplate{},
		}
		p := fake.provider(t)

		md, err := p.GetSourceTemplateMetadata(context.Background())
		require.NoError(t, err)
		require.True(t, namesCluster(md, testCluster))
		require.Equal(t, "gke-current", templateName(p))
	})

	t.Run("resolves a global template", func(t *testing.T) {
		t.Parallel()
		fake := &sourceTemplateFake{
			nodePool:  &container.NodePool{Name: testPool, InstanceGroupUrls: []string{migURL(testZone, "grp-a")}},
			managers:  map[string]string{"grp-a": globalTemplateURL("gke-current")},
			templates: []*compute.InstanceTemplate{current},
			listOnly:  []*compute.InstanceTemplate{},
		}
		p := fake.provider(t)

		_, err := p.GetSourceTemplateMetadata(context.Background())
		require.NoError(t, err)
		require.Equal(t, "gke-current", templateName(p))
	})

	t.Run("takes the newest template when instance groups disagree mid-upgrade", func(t *testing.T) {
		t.Parallel()
		fake := &sourceTemplateFake{
			nodePool: &container.NodePool{Name: testPool, InstanceGroupUrls: []string{
				migURL(testZone, "grp-a"), migURL("us-central1-b", "grp-b"),
			}},
			managers: map[string]string{
				"grp-a": regionalTemplateURL("gke-stale"),
				"grp-b": regionalTemplateURL("gke-current"),
			},
			templates: []*compute.InstanceTemplate{stale, current},
			listOnly:  []*compute.InstanceTemplate{},
		}
		p := fake.provider(t)

		_, err := p.GetSourceTemplateMetadata(context.Background())
		require.NoError(t, err)
		require.Equal(t, "gke-current", templateName(p))
	})

	// Regression for cloudpilot-ai/karpenter-provider-gcp#577: GKE leaves every superseded
	// template in place carrying the pool's label, and the list API does not order its results.
	// Taking the first match handed out a pre-CA-rotation template and the nodes never joined.
	for _, listOrder := range []struct {
		name  string
		items []*compute.InstanceTemplate
	}{
		{name: "newest first", items: []*compute.InstanceTemplate{current, stale}},
		{name: "newest last", items: []*compute.InstanceTemplate{stale, current}},
	} {
		t.Run("label scan takes the newest match, listed "+listOrder.name, func(t *testing.T) {
			t.Parallel()
			fake := &sourceTemplateFake{
				nodePool:      &container.NodePool{Name: testPool, InstanceGroupUrls: []string{migURL(testZone, "grp-a")}},
				managerStatus: http.StatusForbidden,
				templates:     listOrder.items,
			}
			p := fake.provider(t)

			_, err := p.GetSourceTemplateMetadata(context.Background())
			require.NoError(t, err)
			require.Equal(t, "gke-current", templateName(p))
		})
	}

	t.Run("label scan reads past the first page", func(t *testing.T) {
		t.Parallel()
		fake := &sourceTemplateFake{
			nodePool:      &container.NodePool{Name: testPool, InstanceGroupUrls: []string{migURL(testZone, "grp-a")}},
			managerStatus: http.StatusForbidden,
			// The fake serves one template per page, so the only match is on the third page.
			templates: []*compute.InstanceTemplate{
				instanceTemplate("gke-other-pool", testCluster, "other-pool", "2026-08-21T10:00:00.000-07:00"),
				instanceTemplate("gke-other-cluster", "other", testPool, "2026-08-22T10:00:00.000-07:00"),
				current,
			},
		}
		p := fake.provider(t)

		_, err := p.GetSourceTemplateMetadata(context.Background())
		require.NoError(t, err)
		require.Equal(t, "gke-current", templateName(p))
	})

	t.Run("falls back to the label scan when the pool has no instance groups", func(t *testing.T) {
		t.Parallel()
		fake := &sourceTemplateFake{
			nodePool:  &container.NodePool{Name: testPool},
			templates: []*compute.InstanceTemplate{current},
		}
		p := fake.provider(t)

		_, err := p.GetSourceTemplateMetadata(context.Background())
		require.NoError(t, err)
		require.Equal(t, "gke-current", templateName(p))
	})

	t.Run("ignores templates from other clusters", func(t *testing.T) {
		t.Parallel()
		fake := &sourceTemplateFake{
			nodePool:      &container.NodePool{Name: testPool},
			managerStatus: http.StatusForbidden,
			templates: []*compute.InstanceTemplate{
				// Newer, so it would win on timestamp alone if the cluster were not checked.
				instanceTemplate("gke-other-cluster", "other", testPool, "2026-08-24T10:00:00.000-07:00"),
				current,
			},
		}
		p := fake.provider(t)

		md, err := p.GetSourceTemplateMetadata(context.Background())
		require.NoError(t, err)
		require.True(t, namesCluster(md, testCluster))
		require.Equal(t, "gke-current", templateName(p))
	})

	t.Run("errors when neither path finds a template", func(t *testing.T) {
		t.Parallel()
		fake := &sourceTemplateFake{
			nodePool:  &container.NodePool{Name: testPool},
			templates: []*compute.InstanceTemplate{instanceTemplate("gke-other", testCluster, "other-pool", "")},
		}

		_, err := fake.provider(t).GetSourceTemplateMetadata(context.Background())
		require.ErrorContains(t, err, "no instance template found")
	})

	t.Run("errors when the source pool has not been discovered", func(t *testing.T) {
		t.Parallel()
		p := (&sourceTemplateFake{}).provider(t)
		p.sourcePoolName = ""

		_, err := p.GetSourceTemplateMetadata(context.Background())
		require.ErrorContains(t, err, "not yet discovered")
	})
}

func TestParseComputeResourceURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		rawURL     string
		collection string
		want       computeResourceRef
		wantErr    string
	}{
		{
			name:       "zonal instance group manager",
			rawURL:     migURL(testZone, "gke-cluster-pool-abcd1234-grp"),
			collection: "instanceGroupManagers",
			want:       computeResourceRef{project: testProject, scope: scopeZone, location: testZone, name: "gke-cluster-pool-abcd1234-grp"},
		},
		{
			name:       "regional instance template",
			rawURL:     regionalTemplateURL("gke-cluster-pool-abcd1234"),
			collection: "instanceTemplates",
			want:       computeResourceRef{project: testProject, scope: scopeRegion, location: testRegion, name: "gke-cluster-pool-abcd1234"},
		},
		{
			name:       "global instance template",
			rawURL:     globalTemplateURL("gke-cluster-pool-abcd1234"),
			collection: "instanceTemplates",
			want:       computeResourceRef{project: testProject, scope: scopeGlobal, name: "gke-cluster-pool-abcd1234"},
		},
		{
			name:       "regional instance group manager",
			rawURL:     fmt.Sprintf("https://www.googleapis.com/compute/v1/projects/%s/regions/%s/instanceGroupManagers/grp", testProject, testRegion),
			collection: "instanceGroupManagers",
			want:       computeResourceRef{project: testProject, scope: scopeRegion, location: testRegion, name: "grp"},
		},
		{
			name:       "wrong collection",
			rawURL:     migURL(testZone, "grp"),
			collection: "instanceTemplates",
			wantErr:    "does not name a instanceTemplates",
		},
		{
			name:       "missing name",
			rawURL:     fmt.Sprintf("https://www.googleapis.com/compute/v1/projects/%s/zones/%s/instanceGroupManagers", testProject, testZone),
			collection: "instanceGroupManagers",
			wantErr:    "does not name a instanceGroupManagers",
		},
		{
			name:       "unrecognized shape",
			rawURL:     "https://www.googleapis.com/compute/v1/instanceTemplates/tmpl",
			collection: "instanceTemplates",
			wantErr:    "not a recognized instanceTemplates self-link",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseComputeResourceURL(tc.rawURL, tc.collection)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestNewerTemplate(t *testing.T) {
	t.Parallel()
	older := instanceTemplate("older", testCluster, testPool, "2026-08-20T12:00:00.000-04:00") // 16:00Z
	// An hour later in absolute terms but lexicographically smaller: the case a string comparison
	// of creationTimestamp gets wrong.
	newer := instanceTemplate("newer", testCluster, testPool, "2026-08-20T10:00:00.000-07:00") // 17:00Z
	undated := instanceTemplate("undated", testCluster, testPool, "")

	require.Equal(t, newer, newerTemplate(older, newer))
	require.Equal(t, newer, newerTemplate(newer, older))
	require.Equal(t, older, newerTemplate(nil, older))
	require.Equal(t, older, newerTemplate(older, nil))
	require.Nil(t, newerTemplate(nil, nil))
	require.Equal(t, older, newerTemplate(older, undated))
	require.Equal(t, older, newerTemplate(undated, older))
}

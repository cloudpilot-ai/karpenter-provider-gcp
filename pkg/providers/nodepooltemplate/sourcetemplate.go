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
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"google.golang.org/api/compute/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type computeScope string

const (
	scopeGlobal computeScope = "global"
	scopeRegion computeScope = "regions"
	scopeZone   computeScope = "zones"
)

// computeResourceRef is a compute self-link broken into the parts the generated clients take.
type computeResourceRef struct {
	project  string
	scope    computeScope
	location string // zone or region name; empty when the scope is global
	name     string
}

// GetSourceTemplateMetadata returns metadata from the instance template that the selected
// bootstrap source pool is currently running.
//
// GKE builds a new instance template for a node pool on every upgrade and on credential rotation,
// and leaves the superseded ones in place. They all keep the same goog-k8s-node-pool-name label
// and cluster-name metadata, so a label scan cannot tell which one is live; picking a stale one
// bakes an out-of-date cluster CA cert into every node we launch and those nodes never join.
// Ask GKE instead: the node pool names its managed instance groups, and each instance group names
// the template it is running right now.
func (p *DefaultProvider) GetSourceTemplateMetadata(ctx context.Context) (*compute.Metadata, error) {
	sourcePoolName, err := p.getSourcePoolName(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting source pool name: %w", err)
	}

	template, err := p.resolveTemplateFromNodePool(ctx, sourcePoolName)
	if err != nil {
		log.FromContext(ctx).Error(err, "cannot resolve the instance template from the node pool, falling back to a label scan",
			"pool", sourcePoolName)
		if template, err = p.resolveTemplateByLabelScan(ctx, sourcePoolName); err != nil {
			return nil, err
		}
	}

	if template.Properties == nil || template.Properties.Metadata == nil {
		return nil, fmt.Errorf("instance template %q for node pool %q has no metadata", template.Name, sourcePoolName)
	}

	p.mu.Lock()
	previous := p.lastTemplateName
	p.lastTemplateName = template.Name
	p.mu.Unlock()

	logger := log.FromContext(ctx).WithValues("templateName", template.Name,
		"pool", sourcePoolName, "clusterName", p.ClusterInfo.Name)
	if previous != template.Name {
		logger.Info("source instance template selected", "previous", previous)
	} else {
		logger.V(1).Info("Found instance template")
	}
	if !namesCluster(template.Properties.Metadata, p.ClusterInfo.Name) {
		// The node pool is the authority on which template is correct, so this is worth flagging
		// but not worth failing a launch over.
		logger.Info("instance template metadata does not name this cluster")
	}

	return template.Properties.Metadata, nil
}

// resolveTemplateFromNodePool walks NodePool -> managed instance groups -> instance template to
// find the template the pool is running now.
func (p *DefaultProvider) resolveTemplateFromNodePool(ctx context.Context, poolName string) (*compute.InstanceTemplate, error) {
	nodePool, err := p.getNodePool(ctx, poolName)
	if err != nil {
		return nil, fmt.Errorf("getting node pool %q: %w", poolName, err)
	}
	if len(nodePool.InstanceGroupUrls) == 0 {
		return nil, fmt.Errorf("node pool %q has no instance groups", poolName)
	}

	// A multi-zone pool has one instance group per zone, and an instance group part-way through an
	// update names more than one template. Both normally collapse to a single template; where they
	// do not, the newest one is the one being rolled out.
	var templateURLs []string
	var errs []error
	for _, instanceGroupURL := range nodePool.InstanceGroupUrls {
		urls, err := p.instanceGroupTemplateURLs(ctx, instanceGroupURL)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, templateURL := range urls {
			if !slices.Contains(templateURLs, templateURL) {
				templateURLs = append(templateURLs, templateURL)
			}
		}
	}
	if len(templateURLs) == 0 {
		return nil, fmt.Errorf("no instance template named by the instance groups of node pool %q: %w",
			poolName, errors.Join(errs...))
	}

	var newest *compute.InstanceTemplate
	for _, templateURL := range templateURLs {
		template, err := p.getInstanceTemplateByURL(ctx, templateURL)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		newest = newerTemplate(newest, template)
	}
	if newest == nil {
		return nil, fmt.Errorf("fetching instance templates for node pool %q: %w", poolName, errors.Join(errs...))
	}
	return newest, nil
}

// instanceGroupTemplateURLs returns the URLs of the instance templates the given managed instance
// group is running.
//
// A group normally names exactly one, in its top-level instanceTemplate. During an update it also
// populates versions, which the Compute API defines as overriding that top-level field, so versions
// wins wherever it is set. GKE uses a surge upgrade to roll out a new template, including on
// credential rotation, so mid-update a group legitimately names two: the one being replaced and the
// one being rolled out. Both are returned and the caller takes the newest.
func (p *DefaultProvider) instanceGroupTemplateURLs(ctx context.Context, instanceGroupURL string) ([]string, error) {
	ref, err := parseComputeResourceURL(instanceGroupURL, "instanceGroupManagers")
	if err != nil {
		return nil, err
	}
	if ref.scope != scopeZone {
		// GKE node pools always use zonal instance groups, and compute.regionInstanceGroupManagers.get
		// is deliberately not in the controller's IAM role, so there is nothing to gain by calling
		// the regional API here. The label scan covers this if GKE ever changes.
		return nil, fmt.Errorf("instance group %q is not zonal", instanceGroupURL)
	}
	manager, err := p.computeService.InstanceGroupManagers.Get(ref.project, ref.location, ref.name).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("getting instance group manager %q: %w", instanceGroupURL, err)
	}

	var templateURLs []string
	for _, version := range manager.Versions {
		if version != nil && version.InstanceTemplate != "" {
			templateURLs = append(templateURLs, version.InstanceTemplate)
		}
	}
	if len(templateURLs) == 0 && manager.InstanceTemplate != "" {
		templateURLs = append(templateURLs, manager.InstanceTemplate)
	}
	if len(templateURLs) == 0 {
		return nil, fmt.Errorf("instance group manager %q names no instance template", instanceGroupURL)
	}
	return templateURLs, nil
}

// getInstanceTemplateByURL fetches a template from either the regional or the global collection,
// whichever its URL points at. GKE uses regional templates today but has used global ones.
func (p *DefaultProvider) getInstanceTemplateByURL(ctx context.Context, templateURL string) (*compute.InstanceTemplate, error) {
	ref, err := parseComputeResourceURL(templateURL, "instanceTemplates")
	if err != nil {
		return nil, err
	}
	switch ref.scope {
	case scopeRegion:
		return p.computeService.RegionInstanceTemplates.Get(ref.project, ref.location, ref.name).Context(ctx).Do()
	case scopeGlobal:
		return p.computeService.InstanceTemplates.Get(ref.project, ref.name).Context(ctx).Do()
	default:
		return nil, fmt.Errorf("instance template %q is neither regional nor global", templateURL)
	}
}

// resolveTemplateByLabelScan is the degraded path taken when GKE cannot tell us which template the
// pool is running. Every template GKE has ever built for the pool carries the same label, so take
// the most recently created match: after a rotation that is the live one.
func (p *DefaultProvider) resolveTemplateByLabelScan(ctx context.Context, poolName string) (*compute.InstanceTemplate, error) {
	var newest *compute.InstanceTemplate
	err := p.computeService.RegionInstanceTemplates.List(p.ClusterInfo.ProjectID, p.ClusterInfo.Region).
		Pages(ctx, func(page *compute.InstanceTemplateList) error {
			for _, template := range page.Items {
				if p.templateBelongsToPool(template, poolName) {
					newest = newerTemplate(newest, template)
				}
			}
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("cannot list all instance templates for node pool name %q: %w", poolName, err)
	}
	if newest == nil {
		return nil, fmt.Errorf("no instance template found with label %s=%s", nodePoolNameLabelKey, poolName)
	}
	return newest, nil
}

func (p *DefaultProvider) templateBelongsToPool(template *compute.InstanceTemplate, poolName string) bool {
	if template == nil || template.Properties == nil || template.Properties.Labels == nil || template.Properties.Metadata == nil {
		return false
	}
	if !namesCluster(template.Properties.Metadata, p.ClusterInfo.Name) {
		return false
	}
	return template.Properties.Labels[nodePoolNameLabelKey] == poolName
}

// newerTemplate returns whichever of the two templates was created most recently.
func newerTemplate(a, b *compute.InstanceTemplate) *compute.InstanceTemplate {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if templateCreatedAt(b).After(templateCreatedAt(a)) {
		return b
	}
	return a
}

// templateCreatedAt parses a template's creationTimestamp, treating a missing or malformed value
// as the zero time so it sorts oldest. GCP returns RFC3339 with a real UTC offset
// (2025-10-31T11:09:17.350-07:00), so these values cannot be compared as strings.
func templateCreatedAt(template *compute.InstanceTemplate) time.Time {
	parsed, err := time.Parse(time.RFC3339, template.CreationTimestamp)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

// parseComputeResourceURL splits a compute self-link naming a resource in the given collection.
// Self-links take one of three shapes:
//
//	.../projects/{project}/zones/{zone}/{collection}/{name}
//	.../projects/{project}/regions/{region}/{collection}/{name}
//	.../projects/{project}/global/{collection}/{name}
func parseComputeResourceURL(rawURL, collection string) (computeResourceRef, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return computeResourceRef{}, fmt.Errorf("parsing compute URL %q: %w", rawURL, err)
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	i := slices.Index(segments, collection)
	if i < 0 || i == len(segments)-1 {
		return computeResourceRef{}, fmt.Errorf("compute URL %q does not name a %s", rawURL, collection)
	}

	ref := computeResourceRef{name: segments[i+1]}
	switch {
	case i >= 3 && segments[i-1] == string(scopeGlobal) && segments[i-3] == "projects":
		ref.scope = scopeGlobal
		ref.project = segments[i-2]
	case i >= 4 && segments[i-4] == "projects" &&
		(segments[i-2] == string(scopeZone) || segments[i-2] == string(scopeRegion)):
		ref.scope = computeScope(segments[i-2])
		ref.location = segments[i-1]
		ref.project = segments[i-3]
	default:
		return computeResourceRef{}, fmt.Errorf("compute URL %q is not a recognized %s self-link", rawURL, collection)
	}
	return ref, nil
}

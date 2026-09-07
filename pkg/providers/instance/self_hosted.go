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

package instance

import (
	"context"
	"fmt"
	"strings"

	"github.com/samber/lo"
	"google.golang.org/api/compute/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/metadata"
)

// This file contains the self-hosted launch path. Node bootstrap is owned by the
// NodeClass startup script or baked into the image, so the instance carries no GKE
// bootstrap metadata, tags, or labels.

// buildSelfHostedMetadata renders self-hosted instance metadata and validates it
// against GCE's byte limits before Instances.Insert.
func (p *DefaultProvider) buildSelfHostedMetadata(nodeClass *v1alpha1.GCENodeClass) (*compute.Metadata, error) {
	md := metadata.SelfHostedComputeMetadata(p.clusterName, lo.FromPtr(nodeClass.Spec.StartupScript), metadata.CustomMetadata(nodeClass.Spec.Metadata))
	if err := metadata.ValidateComputeMetadataSize(md); err != nil {
		return nil, fmt.Errorf("GCENodeClass %s: %w", nodeClass.Name, err)
	}
	return md, nil
}

// setupSelfHostedNetworkInterfaces builds NICs from spec.networkConfig: the primary
// subnetwork is required and each interface's network is derived via the Compute API.
// Nodes are private by default; alias IP ranges require subnetRangeName since there
// is no cluster IP allocation policy to fall back to.
func (p *DefaultProvider) setupSelfHostedNetworkInterfaces(ctx context.Context, nodeClass *v1alpha1.GCENodeClass) ([]*compute.NetworkInterface, error) {
	cfg := nodeClass.Spec.NetworkConfig
	if cfg == nil || cfg.Subnetwork == "" {
		return nil, fmt.Errorf("GCENodeClass %s: spec.networkConfig.subnetwork is required in self-hosted mode", nodeClass.Name)
	}
	project, region, name, err := parseSubnetworkRef(cfg.Subnetwork, p.projectID, p.region)
	if err != nil {
		return nil, err
	}
	network, err := p.networkForSubnetwork(ctx, project, region, name)
	if err != nil {
		return nil, err
	}

	disableExternal := cfg.EnablePrivateNodes == nil || *cfg.EnablePrivateNodes

	primary := &compute.NetworkInterface{
		Network:    network,
		Subnetwork: canonicalSubnetworkRef(project, region, name),
	}
	if rangeName := lo.FromPtr(nodeClass.Spec.SubnetRangeName); rangeName != "" {
		primary.AliasIpRanges = []*compute.AliasIpRange{{
			IpCidrRange:         fmt.Sprintf("/%d", podCIDRRange(nodeClass.GetMaxPods())),
			SubnetworkRangeName: rangeName,
		}}
	}
	applyAccessConfig(primary, disableExternal)

	ifaces := []*compute.NetworkInterface{primary}
	for _, override := range cfg.AdditionalNetworkInterfaces {
		project, region, name, err := parseSubnetworkRef(override.Subnetwork, p.projectID, p.region)
		if err != nil {
			return nil, err
		}
		ifaceNetwork := override.Network
		if ifaceNetwork == "" {
			if ifaceNetwork, err = p.networkForSubnetwork(ctx, project, region, name); err != nil {
				return nil, err
			}
		}
		iface := &compute.NetworkInterface{
			Network: ifaceNetwork,
			// The raw spec value may be a bare name, which Instances.Insert rejects.
			Subnetwork: canonicalSubnetworkRef(project, region, name),
		}
		applyAccessConfig(iface, disableExternal)
		ifaces = append(ifaces, iface)
	}
	return ifaces, nil
}

// networkForSubnetwork returns the subnetwork's network, caching the effectively
// immutable mapping so launch retries do not re-fetch it.
func (p *DefaultProvider) networkForSubnetwork(ctx context.Context, project, region, name string) (string, error) {
	ref := canonicalSubnetworkRef(project, region, name)
	if network, ok := p.subnetworkNetworks.Load(ref); ok {
		return network.(string), nil
	}
	subnet, err := p.computeService.Subnetworks.Get(project, region, name).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("fetching subnetwork %q to derive its network: %w", ref, err)
	}
	p.subnetworkNetworks.Store(ref, subnet.Network)
	return subnet.Network, nil
}

// canonicalSubnetworkRef builds the partial-URL subnetwork reference accepted by
// Instances.Insert.
func canonicalSubnetworkRef(project, region, name string) string {
	return fmt.Sprintf("projects/%s/regions/%s/subnetworks/%s", project, region, name)
}

// parseSubnetworkRef accepts a full URL, a partial path with projects/ and/or
// regions/ segments, or a bare name resolved against the operator's project and region.
func parseSubnetworkRef(value, defaultProject, defaultRegion string) (project, region, name string, err error) {
	project, region = defaultProject, defaultRegion
	if !strings.Contains(value, "/") {
		return project, region, value, nil
	}
	parts := strings.Split(strings.Trim(value, "/"), "/")
	for i := 0; i < len(parts)-1; i++ {
		switch parts[i] {
		case "projects":
			project = parts[i+1]
		case "regions":
			region = parts[i+1]
		case "subnetworks":
			name = parts[i+1]
		}
	}
	if name == "" {
		return "", "", "", fmt.Errorf("invalid subnetwork reference %q: expected a bare name or a path containing subnetworks/<name>", value)
	}
	return project, region, name, nil
}

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

package metadata

import (
	"context"

	"google.golang.org/api/compute/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/nodepooltemplate"
)

const (
	ClusterNameLabel     = "cluster-name"
	GKENodePoolLabel     = "cloud.google.com/gke-nodepool"
	UnregisteredTaintArg = "--register-with-taints=karpenter.sh/unregistered=true:NoSchedule"
)

type Metadata struct {
	nodePoolTemplateProvider nodepooltemplate.Provider
}

func NewMetadata(nodePoolTemplateProvider nodepooltemplate.Provider) *Metadata {
	return &Metadata{
		nodePoolTemplateProvider: nodePoolTemplateProvider,
	}
}

// ResolveMetadata resolves the metadata for the node pool template
// In default nodepool, gke will create a default nodepool template with default custom metadata.
// We need to make sure the label `cloud.google.com/gke-nodepool=cluster-name` in metadata is removed.
func (m *Metadata) ResolveMetadata(ctx context.Context) (map[string]*compute.Metadata, error) {
	instanceTemplates, err := m.nodePoolTemplateProvider.GetInstanceTemplates(ctx)
	if err != nil {
		return nil, err
	}

	metadataMap := map[string]*compute.Metadata{}
	for _, instanceTemplate := range instanceTemplates {
		if err := RemoveGKEBuiltinLabels(instanceTemplate.Properties.Metadata); err != nil {
			return nil, err
		}
		metadataMap[instanceTemplate.Name] = instanceTemplate.Properties.Metadata
	}

	return metadataMap, nil
}

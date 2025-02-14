/*
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

package cloudprovider

import (
	"context"
	"fmt"

	"github.com/cloudpilot-ai/karpenter-provider-alibabacloud/pkg/utils"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/util/sets"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/instance"
)

const (
	SecurityGroupDrift cloudprovider.DriftReason = "SecurityGroupDrift"
	NodeClassDrift     cloudprovider.DriftReason = "NodeClassDrift"
)

func (c *CloudProvider) isNodeClassDrifted(ctx context.Context, nodeClaim *karpv1.NodeClaim, _ *karpv1.NodePool, nodeClass *v1alpha1.GCENodeClass) (cloudprovider.DriftReason, error) {
	// First check if the node class is statically drifted to save on API calls.
	if drifted := c.areStaticFieldsDrifted(nodeClaim, nodeClass); drifted != "" {
		return drifted, nil
	}
	instance, err := c.getInstance(ctx, nodeClaim.Status.ProviderID)
	if err != nil {
		return "", err
	}
	securityGroupDrifted, err := c.areSecurityGroupsDrifted(instance, nodeClass)
	if err != nil {
		return "", fmt.Errorf("calculating securitygroup drift, %w", err)
	}
	drifted := lo.FindOrElse([]cloudprovider.DriftReason{securityGroupDrifted}, "", func(i cloudprovider.DriftReason) bool {
		return string(i) != ""
	})
	return drifted, nil
}

func (c *CloudProvider) areStaticFieldsDrifted(nodeClaim *karpv1.NodeClaim, nodeClass *v1alpha1.GCENodeClass) cloudprovider.DriftReason {
	nodeClassHash, foundNodeClassHash := nodeClass.Annotations[v1alpha1.AnnotationGCENodeClassHash]
	nodeClassHashVersion, foundNodeClassHashVersion := nodeClass.Annotations[v1alpha1.AnnotationGCENodeClassHashVersion]
	nodeClaimHash, foundNodeClaimHash := nodeClaim.Annotations[v1alpha1.AnnotationGCENodeClassHash]
	nodeClaimHashVersion, foundNodeClaimHashVersion := nodeClaim.Annotations[v1alpha1.AnnotationGCENodeClassHashVersion]

	if !foundNodeClassHash || !foundNodeClaimHash || !foundNodeClassHashVersion || !foundNodeClaimHashVersion {
		return ""
	}
	// validate that the hash version for the GCENodeClass is the same as the NodeClaim before evaluating for static drift
	if nodeClassHashVersion != nodeClaimHashVersion {
		return ""
	}
	return lo.Ternary(nodeClassHash != nodeClaimHash, NodeClassDrift, "")
}

// Checks if the security groups are drifted, by comparing the security groups returned from the SecurityGroupProvider
// to the GCE instance security groups
func (c *CloudProvider) areSecurityGroupsDrifted(GCEInstance *instance.Instance, nodeClass *v1alpha1.GCENodeClass) (cloudprovider.DriftReason, error) {
	securityGroupIds := sets.New(lo.Map(nodeClass.Status.SecurityGroups, func(sg v1alpha1.SecurityGroup, _ int) string { return sg.ID })...)
	if len(securityGroupIds) == 0 {
		return "", fmt.Errorf("no security groups are present in the status")
	}

	if !securityGroupIds.Equal(sets.New(GCEInstance.SecurityGroupIDs...)) {
		return SecurityGroupDrift, nil
	}
	return "", nil
}

func (c *CloudProvider) getInstance(ctx context.Context, providerID string) (*instance.Instance, error) {
	// Get InstanceID to fetch from GCE
	instanceID, err := utils.ParseInstanceID(providerID)
	if err != nil {
		return nil, err
	}
	instance, err := c.instanceProvider.Get(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("getting instance, %w", err)
	}
	return instance, nil
}

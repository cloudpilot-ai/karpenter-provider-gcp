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
	"net/http"
	"time"

	"github.com/awslabs/operatorpkg/status"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

const CloudProviderName = "gcp"

var _ cloudprovider.CloudProvider = (*CloudProvider)(nil)

type CloudProvider struct {
	kubeClient client.Client
	recorder   events.Recorder
}

func New(kubeClient client.Client,
	recorder events.Recorder,
) *CloudProvider {
	return &CloudProvider{
		kubeClient: kubeClient,
		recorder:   recorder,
	}
}

// Create a NodeClaim given the constraints.
func (c *CloudProvider) Create(ctx context.Context, nodeClaim *karpv1.NodeClaim) (*karpv1.NodeClaim, error) {
	return nil, fmt.Errorf("not implemented")
}

func (c *CloudProvider) List(ctx context.Context) ([]*karpv1.NodeClaim, error) {
	return nil, fmt.Errorf("not implemented")

}

func (c *CloudProvider) Get(ctx context.Context, providerID string) (*karpv1.NodeClaim, error) {
	return nil, fmt.Errorf("not implemented")

}

func (c *CloudProvider) LivenessProbe(req *http.Request) error {
	return nil
}

// GetInstanceTypes returns all available InstanceTypes
func (c *CloudProvider) GetInstanceTypes(ctx context.Context, nodePool *karpv1.NodePool) ([]*cloudprovider.InstanceType, error) {
	return nil, fmt.Errorf("not implemented")
}

func (c *CloudProvider) Delete(ctx context.Context, nodeClaim *karpv1.NodeClaim) error {
	return fmt.Errorf("not implemented")
}

func (c *CloudProvider) IsDrifted(ctx context.Context, nodeClaim *karpv1.NodeClaim) (cloudprovider.DriftReason, error) {
	return "", fmt.Errorf("not implemented")
}

func (c *CloudProvider) RepairPolicies() []cloudprovider.RepairPolicy {
	return []cloudprovider.RepairPolicy{
		// Supported Kubelet Node Conditions
		{
			ConditionType:      corev1.NodeReady,
			ConditionStatus:    corev1.ConditionFalse,
			TolerationDuration: 30 * time.Minute,
		},
		{
			ConditionType:      corev1.NodeReady,
			ConditionStatus:    corev1.ConditionUnknown,
			TolerationDuration: 30 * time.Minute,
		},
		// Support Node Monitoring Agent Conditions
		//
		{
			ConditionType:      "AcceleratedHardwareReady",
			ConditionStatus:    corev1.ConditionFalse,
			TolerationDuration: 10 * time.Minute,
		},
		{
			ConditionType:      "StorageReady",
			ConditionStatus:    corev1.ConditionFalse,
			TolerationDuration: 30 * time.Minute,
		},
		{
			ConditionType:      "NetworkingReady",
			ConditionStatus:    corev1.ConditionFalse,
			TolerationDuration: 30 * time.Minute,
		},
		{
			ConditionType:      "KernelReady",
			ConditionStatus:    corev1.ConditionFalse,
			TolerationDuration: 30 * time.Minute,
		},
		{
			ConditionType:      "ContainerRuntimeReady",
			ConditionStatus:    corev1.ConditionFalse,
			TolerationDuration: 30 * time.Minute,
		},
	}
}

// Name returns the CloudProvider implementation name.
func (c *CloudProvider) Name() string {
	return CloudProviderName
}

func (c *CloudProvider) GetSupportedNodeClasses() []status.Object {
	return []status.Object{&v1alpha1.GCENodeClass{}}
}

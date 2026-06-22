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

package controllers

import (
	"context"

	"github.com/awslabs/operatorpkg/controller"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	metricsclientset "k8s.io/metrics/pkg/client/clientset/versioned"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/cloudprovider"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/controllers/csr"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/controllers/interruption"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/controllers/node"
	nodeclaimgc "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/controllers/nodeclaim/garbagecollection"
	nodeclasshash "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/controllers/nodeclass/hash"
	nodeclassstatus "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/controllers/nodeclass/status"
	nodeclasstermination "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/controllers/nodeclass/termination"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/controllers/nodepooltemplate"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/controllers/providers/instancetype"
	controllerspricing "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/controllers/providers/pricing"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/controllers/telemetry"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/operator/options"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/imagefamily"
	providerinstancetype "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/instancetype"
	providernodepooltemplate "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/nodepooltemplate"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/offerings/unavailableofferings"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/pricing"
)

func NewController(
	ctx context.Context,
	restConfig *rest.Config,
	kubeClient client.Client,
	kubernetesInterface kubernetes.Interface,
	recorder events.Recorder,
	unavailableOfferings *unavailableofferings.UnavailableOfferings,
	imageProvider imagefamily.Provider,
	nodePoolTemplateProvider providernodepooltemplate.Provider,
	instanceTypeProvider providerinstancetype.Provider,
	cloudProvider *cloudprovider.CloudProvider,
	pricingProvider pricing.Provider,
	clk clock.Clock,
	cluster *state.Cluster,
) []controller.Controller {
	controllers := []controller.Controller{
		nodeclassstatus.NewController(kubeClient, imageProvider),
		nodepooltemplate.NewController(nodePoolTemplateProvider),
		nodeclasstermination.NewController(kubeClient),
		nodeclasshash.NewController(kubeClient),
		instancetype.NewController(instanceTypeProvider),
		csr.NewController(kubernetesInterface),
		controllerspricing.NewController(pricingProvider),
		node.NewController(kubeClient, cloudProvider, clk, cluster, recorder),
		nodeclaimgc.NewController(kubeClient, cloudProvider),
	}

	if options.FromContext(ctx).Interruption {
		controllers = append(controllers, interruption.NewController(
			kubeClient,
			recorder,
			unavailableOfferings,
		))
	}

	controllers = append(controllers, telemetry.NewController(
		kubeClient,
		metricsclientset.NewForConfigOrDie(restConfig),
	))

	return controllers
}

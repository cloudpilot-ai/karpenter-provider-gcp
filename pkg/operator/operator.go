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

package operator

import (
	"context"
	"os"

	"github.com/samber/lo"
	"google.golang.org/api/compute/v1"
	container "google.golang.org/api/container/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/operator"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/auth"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/operator/options"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/gke"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/imagefamily"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/instance"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/instancetype"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/nodepooltemplate"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/offerings/unavailableofferings"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/pricing"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/pricing/instanceprice"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/version"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
)

func init() {
	karpv1.NormalizedLabels = lo.Assign(karpv1.NormalizedLabels, map[string]string{"topology.gke.io/zone": corev1.LabelTopologyZone})
}

type Operator struct {
	*operator.Operator

	UnavailableOfferingsCache *unavailableofferings.UnavailableOfferings
	ImagesProvider            imagefamily.Provider
	NodePoolTemplateProvider  nodepooltemplate.Provider
	PricingProvider           pricing.Provider
	InstanceTypeProvider      instancetype.Provider
	InstanceProvider          instance.Provider
}

func NewOperator(ctx context.Context, operator *operator.Operator) (context.Context, *Operator) {
	os.Setenv(options.GCPAuth, options.FromContext(ctx).GCPAuth)

	region := utils.RegionFromLocation(options.FromContext(ctx).ClusterLocation)
	log.FromContext(ctx).Info("determined region", "Region", region, "Location", options.FromContext(ctx).ClusterLocation)

	computeService, err := compute.NewService(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to create compute service")
		os.Exit(1)
	}

	// Self-hosted mode never calls the Container API, so the container service and the
	// GKE bootstrap template provider are not built.
	selfHosted := options.FromContext(ctx).IsSelfHosted()
	var containerService *container.Service
	var nodeTemplateProvider nodepooltemplate.Provider
	if !selfHosted {
		containerService, err = container.NewService(ctx)
		if err != nil {
			log.FromContext(ctx).Error(err, "failed to create container service")
			os.Exit(1)
		}
	}
	auth := auth.Credential{
		ProjectID:    options.FromContext(ctx).ProjectID,
		Region:       region,
		NodeLocation: options.FromContext(ctx).NodeLocation,
	}

	versionProvider := version.NewDefaultProvider(operator.KubernetesInterface)
	if !selfHosted {
		nodeTemplateProvider = nodepooltemplate.NewDefaultProvider(
			ctx,
			computeService,
			containerService,
			options.FromContext(ctx).ClusterName,
			region,
			options.FromContext(ctx).ProjectID,
			options.FromContext(ctx).NodePoolServiceAccount,
			options.FromContext(ctx).ClusterLocation,
			options.FromContext(ctx).NodeLocation,
			options.FromContext(ctx).DefaultNodePoolTemplateName,
		)
	}
	billingClient, err := instanceprice.New(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "Failed to create GCP billing client")
		os.Exit(1)
	}
	pricingProvider, err := pricing.NewDefaultProvider(ctx, billingClient, region)
	if err != nil {
		log.FromContext(ctx).Error(err, "Failed to create pricing provider")
		os.Exit(1)
	}

	unavailableOfferingsCache := unavailableofferings.NewUnavailableOfferings()
	var gkeProvider gke.Provider
	if selfHosted {
		gkeProvider = gke.NewSelfHostedProvider(computeService, options.FromContext(ctx).ProjectID, region)
	} else {
		gkeProvider = gke.NewDefaultProvider(computeService, containerService,
			options.FromContext(ctx).ProjectID,
			options.FromContext(ctx).NodeLocation,
			options.FromContext(ctx).ClusterName,
		)
	}
	imageProvider := imagefamily.NewDefaultProvider(computeService, versionProvider, gkeProvider)

	projectID := options.FromContext(ctx).ProjectID
	computeDefaultSA := ""
	if proj, err := computeService.Projects.Get(projectID).Context(ctx).Do(); err != nil {
		log.FromContext(ctx).Error(err, "failed to get Compute Engine default service account; nodes without an explicit serviceAccount will fail to provision")
	} else {
		computeDefaultSA = proj.DefaultServiceAccount
	}

	instanceProvider := instance.NewProvider(
		options.FromContext(ctx).ClusterName,
		options.FromContext(ctx).ClusterLocation,
		region,
		projectID,
		options.FromContext(ctx).NodePoolServiceAccount,
		computeDefaultSA,
		computeService,
		gkeProvider,
		nodeTemplateProvider,
		versionProvider,
		unavailableOfferingsCache,
	)
	instanceTypeProvider := instancetype.NewDefaultProvider(ctx, &auth, pricingProvider, gkeProvider, unavailableOfferingsCache)

	return ctx, &Operator{
		Operator:                  operator,
		UnavailableOfferingsCache: unavailableOfferingsCache,
		ImagesProvider:            imageProvider,
		NodePoolTemplateProvider:  nodeTemplateProvider,
		PricingProvider:           pricingProvider,
		InstanceTypeProvider:      instanceTypeProvider,
		InstanceProvider:          instanceProvider,
	}
}

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
	"net/http"
	"os"

	computev1 "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/metadata"
	containerapiv1 "cloud.google.com/go/container/apiv1"
	"github.com/samber/lo"
	"google.golang.org/api/compute/v1"
	container "google.golang.org/api/container/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/operator"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/auth"
	pkgcache "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/cache"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/operator/options"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/gke"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/imagefamily"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/instance"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/instancetype"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/nodepooltemplate"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/pricing"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/version"
)

func init() {
	karpv1.NormalizedLabels = lo.Assign(karpv1.NormalizedLabels, map[string]string{"topology.gke.io/zone": corev1.LabelTopologyZone})
}

type Operator struct {
	*operator.Operator

	Credential                auth.Credential
	UnavailableOfferingsCache *pkgcache.UnavailableOfferings
	MetadataClient            *metadata.Client
	ZoneOperationClient       *computev1.ZoneOperationsClient
	ImagesProvider            imagefamily.Provider
	NodePoolTemplateProvider  nodepooltemplate.Provider
	PricingProvider           pricing.Provider
	InstanceTypeProvider      instancetype.Provider
	InstanceProvider          instance.Provider
}

func NewOperator(ctx context.Context, operator *operator.Operator) (context.Context, *Operator) {
	os.Setenv(options.GCPAuth, options.FromContext(ctx).GCPAuth)

	computeService, err := compute.NewService(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to create compute service")
		os.Exit(1)
	}
	containerService, err := container.NewService(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to create container service")
		os.Exit(1)
	}
	auth := auth.Credential{
		ProjectID: options.FromContext(ctx).ProjectID,
		Region:    options.FromContext(ctx).Region,
	}

	versionProvider := version.NewDefaultProvider(operator.KubernetesInterface)
	nodeTemplateProvider := nodepooltemplate.NewDefaultProvider(
		ctx,
		operator.GetClient(),
		computeService,
		containerService,
		versionProvider,
		options.FromContext(ctx).ClusterName,
		options.FromContext(ctx).Region,
		options.FromContext(ctx).ProjectID,
		options.FromContext(ctx).NodePoolServiceAccount,
	)
	imageProvider := imagefamily.NewDefaultProvider(computeService, nodeTemplateProvider)
	pricingProvider, err := pricing.NewDefaultProvider(ctx, options.FromContext(ctx).Region)
	if err != nil {
		log.FromContext(ctx).Error(err, "Failed to create pricing provider")
		os.Exit(1)
	}

	unavailableOfferingsCache := pkgcache.NewUnavailableOfferings()
	metadataClient := metadata.NewClient(http.DefaultClient)
	zoneOperationClient, err := computev1.NewZoneOperationsRESTClient(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "Failed to create zone operation client")
		os.Exit(1)
	}
	gkeClient, err := containerapiv1.NewClusterManagerClient(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to create gke client")
		os.Exit(1)
	}
	gkeProvider := gke.NewDefaultProvider(gkeClient)

	instanceProvider := instance.NewProvider(
		options.FromContext(ctx).ClusterName,
		options.FromContext(ctx).Region,
		options.FromContext(ctx).ProjectID,
		options.FromContext(ctx).NodePoolServiceAccount,
		computeService,
		gkeProvider,
		unavailableOfferingsCache,
		operator.GetClient(),
	)
	instanceTypeProvider := instancetype.NewDefaultProvider(ctx, &auth, pricingProvider, gkeProvider, unavailableOfferingsCache)

	return ctx, &Operator{
		Operator:                  operator,
		Credential:                auth,
		UnavailableOfferingsCache: unavailableOfferingsCache,
		MetadataClient:            metadataClient,
		ZoneOperationClient:       zoneOperationClient,
		ImagesProvider:            imageProvider,
		NodePoolTemplateProvider:  nodeTemplateProvider,
		PricingProvider:           pricingProvider,
		InstanceTypeProvider:      instanceTypeProvider,
		InstanceProvider:          instanceProvider,
	}
}

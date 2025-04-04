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

	"google.golang.org/api/compute/v1"
	"google.golang.org/api/container/v1"
	"google.golang.org/api/option"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/karpenter/pkg/operator"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/imagefamily"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/nodepooltemplate"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/version"
)

func init() {
}

type Operator struct {
	*operator.Operator

	ImagesProvider           imagefamily.Provider
	NodePoolTemplateProvider nodepooltemplate.Provider
}

func NewOperator(ctx context.Context, operator *operator.Operator) (context.Context, *Operator) {
	computeService, err := compute.NewService(ctx, option.WithCredentialsFile("demo/account.json"))
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to create compute service")
		os.Exit(1)
	}
	containerService, err := container.NewService(ctx, option.WithCredentialsFile("demo/account.json"))
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to create container service")
		os.Exit(1)
	}

	versionProvider := version.NewDefaultProvider(operator.KubernetesInterface)
	nodeTemplateProvider := nodepooltemplate.NewDefaultProvider(operator.GetClient(),
		computeService, containerService, versionProvider)
	imageProvider := imagefamily.NewDefaultProvider(versionProvider, nodeTemplateProvider)

	return ctx, &Operator{
		Operator:                 operator,
		ImagesProvider:           imageProvider,
		NodePoolTemplateProvider: nodeTemplateProvider,
	}
}

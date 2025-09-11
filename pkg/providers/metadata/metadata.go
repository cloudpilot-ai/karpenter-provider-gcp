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
	"fmt"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/nodepooltemplate"
)

const (
	ClusterNameLabel     = "cluster-name"
	UnregisteredTaintArg = "--register-with-taints=karpenter.sh/unregistered=true:NoExecute"
	KubeletConfigLabel   = "kubelet-config"
)

var (
	RegisteredLabel = fmt.Sprintf("%s=%s", karpv1.NodeRegisteredLabelKey, "true")
)

type Metadata struct {
	nodePoolTemplateProvider nodepooltemplate.Provider
}

func NewMetadata(nodePoolTemplateProvider nodepooltemplate.Provider) *Metadata {
	return &Metadata{
		nodePoolTemplateProvider: nodePoolTemplateProvider,
	}
}

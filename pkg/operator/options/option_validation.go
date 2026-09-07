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

package options

import (
	"fmt"

	"go.uber.org/multierr"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
)

func (o *Options) Validate() error {
	return multierr.Combine(
		o.validateRequiredFields(),
		o.validateProvisionMode(),
	)
}

func (o *Options) validateProvisionMode() error {
	if o.ProvisionMode == ProvisionModeGKE {
		return nil
	}
	if o.ProvisionMode != ProvisionModeSelfHosted {
		return fmt.Errorf("invalid value %q for flag %s or env var %s, allowed values are %q and %q",
			o.ProvisionMode, provisionModeFlagName, provisionModeEnvVarName, ProvisionModeGKE, ProvisionModeSelfHosted)
	}
	// Bootstrap-pool pinning is meaningless without GKE node pools.
	if o.DefaultNodePoolTemplateName != "" {
		return fmt.Errorf("flag %s or env var %s cannot be set when %s is %q",
			defaultNodePoolTemplateNameFlagName, defaultNodePoolTemplateNameEnvVarName, provisionModeFlagName, ProvisionModeSelfHosted)
	}
	// The GC label carries the sanitized cluster name; startup fails if sanitization
	// would produce an empty value.
	if utils.SanitizeGCELabelValue(o.ClusterName) == "" {
		return fmt.Errorf("cluster name %q sanitizes to an empty GCE label value; set a %s containing at least one alphanumeric character",
			o.ClusterName, gkeClusterFlagName)
	}
	return nil
}

func (o *Options) validateRequiredFields() error {
	if o.ProjectID == "" {
		return fmt.Errorf("missing required flag %s or env var %s", projectIDFlagName, projectIDEnvVarName)
	}
	if o.ClusterName == "" {
		return fmt.Errorf("missing required flag %s or env var %s", gkeClusterFlagName, gkeClusterNameEnvVarName)
	}
	if o.ClusterLocation == "" {
		return fmt.Errorf("missing required flag %s or env var %s", clusterLocationFlagName, clusterLocationEnvVarName)
	}
	return nil
}

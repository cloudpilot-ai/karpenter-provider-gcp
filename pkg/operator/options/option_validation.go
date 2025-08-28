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
)

func (o *Options) Validate() error {
	return multierr.Combine(
		o.validateRequiredFields(),
	)
}

func (o *Options) validateRequiredFields() error {
	if o.ProjectID == "" {
		return fmt.Errorf("missing required flag %s or env var %s", projectIDFlagName, projectIDEnvVarName)
	}
	if o.ClusterName == "" {
		return fmt.Errorf("missing required flag %s or env var %s", gkeClusterFlagName, gkeClusterNameEnvVarName)
	}
	if o.Location == "" {
		return fmt.Errorf("missing required flag %s or env var %s", locationFlagName, locationEnvVarName)
	}
	return nil
}

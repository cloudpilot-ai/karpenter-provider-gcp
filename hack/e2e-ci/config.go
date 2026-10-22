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

package e2eci

import (
	"errors"
	"regexp"
)

// Configuration is captured from fixed, reviewed repository variables at the
// gate. Runtime target authority never comes from dispatch or build artifacts.
type Configuration struct {
	Enabled        bool              `json:"enabled"`
	Presets        []string          `json:"presets"`
	Workers        int               `json:"workers"`
	Provider       string            `json:"provider"`
	Audience       string            `json:"audience"`
	ServiceAccount string            `json:"service_account"`
	Target         map[string]string `json:"target"`
}

var targetKeys = []string{"E2E_PROJECT_ID", "E2E_LOCATION", "E2E_REGION", "E2E_PREFIX", "E2E_CLUSTER_NAME", "E2E_IMAGE_REPOSITORY", "E2E_CONTROLLER_SERVICE_ACCOUNT", "E2E_NODE_SERVICE_ACCOUNT", "E2E_PODS_RANGE_NAME", "E2E_SMALL_PODS_RANGE_NAME", "E2E_LARGE_PODS_RANGE_NAME", "E2E_RESOURCE_MANAGER_TAG_KEY"}

func (c Configuration) Validate(preset string) error {
	if !c.Enabled || c.Workers != 20 {
		return errors.New("CI disabled or worker cap is not 20")
	}
	enabled := false
	for _, p := range c.Presets {
		if p != "standard" && p != "gpu" && p != "full" {
			return errors.New("invalid enabled preset")
		}
		enabled = enabled || p == preset
	}
	if !enabled {
		return errors.New("preset disabled")
	}
	if len(c.Target) != len(targetKeys) {
		return errors.New("unexpected target configuration")
	}
	safe := regexp.MustCompile(`^[a-zA-Z0-9@/._-]{1,256}$`)
	for _, key := range targetKeys {
		if !safe.MatchString(c.Target[key]) {
			return errors.New("missing or malformed target field " + key)
		}
	}
	if !regexp.MustCompile(`^projects/[0-9]+/locations/global/workloadIdentityPools/[a-z0-9-]+/providers/github$`).MatchString(c.Provider) || c.Audience != "https://iam.googleapis.com/"+c.Provider {
		return errors.New("invalid explicit WIF provider/audience")
	}
	if c.ServiceAccount != c.Target["E2E_PREFIX"]+"-ci-sa@"+c.Target["E2E_PROJECT_ID"]+".iam.gserviceaccount.com" {
		return errors.New("runtime service account mismatch")
	}
	return nil
}

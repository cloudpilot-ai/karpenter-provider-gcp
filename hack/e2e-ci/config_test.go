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

import "testing"

func TestAdministrativeConfigurationFailsClosed(t *testing.T) {
	var c Configuration
	if c.Validate("standard") == nil {
		t.Fatal("missing administrative configuration enabled runtime")
	}
}

func administrativeFixture() Configuration {
	return Configuration{Enabled: true, Workers: 20, Presets: []string{"standard"}, Provider: "projects/123/locations/global/workloadIdentityPools/e2e-ci/providers/github", Audience: "https://iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/e2e-ci/providers/github", ServiceAccount: "e2e-ci-sa@fixture-project.iam.gserviceaccount.com", Target: map[string]string{"E2E_PROJECT_ID": "fixture-project", "E2E_LOCATION": "us-central1", "E2E_REGION": "us-central1", "E2E_PREFIX": "e2e", "E2E_CLUSTER_NAME": "e2e-cluster", "E2E_IMAGE_REPOSITORY": "us-central1-docker.pkg.dev/fixture-project/e2e-images/karpenter", "E2E_CONTROLLER_SERVICE_ACCOUNT": "e2e-karpenter@fixture-project.iam.gserviceaccount.com", "E2E_NODE_SERVICE_ACCOUNT": "e2e-nodes@fixture-project.iam.gserviceaccount.com", "E2E_PODS_RANGE_NAME": "pods", "E2E_SMALL_PODS_RANGE_NAME": "small", "E2E_LARGE_PODS_RANGE_NAME": "large", "E2E_RESOURCE_MANAGER_TAG_KEY": "e2e-tag"}}
}
func TestAdministrativeConfigurationFreezesExplicitTargetAndCap(t *testing.T) {
	if err := administrativeFixture().Validate("standard"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*Configuration)
	}{
		{"disabled", func(c *Configuration) { c.Enabled = false }}, {"workers", func(c *Configuration) { c.Workers = 19 }}, {"missing range", func(c *Configuration) { delete(c.Target, "E2E_SMALL_PODS_RANGE_NAME") }}, {"extra authority", func(c *Configuration) { c.Target["SHELL"] = "bash" }}, {"newline", func(c *Configuration) { c.Target["E2E_PROJECT_ID"] = "fixture\nGH_TOKEN=x" }}, {"wrong audience", func(c *Configuration) { c.Audience = c.Provider }}, {"wrong SA", func(c *Configuration) { c.ServiceAccount = "controller@fixture-project.iam.gserviceaccount.com" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := administrativeFixture()
			tc.change(&c)
			if c.Validate("standard") == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	if administrativeFixture().Validate("gpu") == nil || administrativeFixture().Validate("full") == nil {
		t.Fatal("unvalidated preset enabled")
	}
}

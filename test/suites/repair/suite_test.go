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

package repair_test

import (
	"context"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

var env *environment.Environment

func TestRepair(t *testing.T) {
	RegisterFailHandler(Fail)
	BeforeSuite(func() {
		env = environment.NewEnvironment()
		requireNodeRepairEnabled()
	})
	AfterSuite(func() {
		if env != nil {
			env.Cleanup()
		}
	})
	RunSpecs(t, "Repair Suite")
}

// requireNodeRepairEnabled fails the suite immediately if the NodeRepair feature gate is not
// enabled in the running karpenter deployment. This avoids spending 25m waiting for a repair
// that will never happen when the gate is off or the deployment is misconfigured.
func requireNodeRepairEnabled() {
	dep, err := env.KubeClient.AppsV1().Deployments(environment.KarpenterNamespace).
		Get(context.Background(), environment.KarpenterDeployment, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred(), "failed to get karpenter deployment")
	for _, c := range dep.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == "FEATURE_GATES" {
				Expect(strings.Contains(e.Value, "NodeRepair=true")).To(BeTrue(),
					"NodeRepair=true must be set in FEATURE_GATES; got %q — redeploy with --set controller.featureGates.nodeRepair=true", e.Value)
				return
			}
		}
	}
	Fail("FEATURE_GATES env var not found in karpenter deployment — is the controller deployed?")
}

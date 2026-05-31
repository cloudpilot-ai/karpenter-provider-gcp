/*
Copyright 2026 The CloudPilot AI Authors.

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

package confidential_test

import (
	"context"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

// confidentialCase maps a confidential technology to a machine family and
// instance types known to support it.
type confidentialCase struct {
	confidentialType string
	families         []string
	instanceTypes    []string
}

var confidentialCases = map[string]confidentialCase{
	"SEV":     {confidentialType: "SEV", families: []string{"n2d"}, instanceTypes: []string{"n2d-standard-2", "n2d-standard-4"}},
	"SEV_SNP": {confidentialType: "SEV_SNP", families: []string{"n2d"}, instanceTypes: []string{"n2d-standard-2", "n2d-standard-4"}},
	"TDX":     {confidentialType: "TDX", families: []string{"c3"}, instanceTypes: []string{"c3-standard-4", "c3-standard-8"}},
}

// enabledConfidentialTypes returns the confidential types to exercise, from
// E2E_CONFIDENTIAL_TYPES (comma-separated). Defaults to SEV,SEV_SNP, which only
// require n2d capacity; add TDX where c3 capacity is available.
func enabledConfidentialTypes() []string {
	raw := os.Getenv("E2E_CONFIDENTIAL_TYPES")
	if raw == "" {
		raw = "SEV,SEV_SNP"
	}
	var out []string
	for _, t := range strings.Split(raw, ",") {
		t = strings.TrimSpace(t)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

var _ = Describe("Confidential", func() {
	for _, confidentialType := range enabledConfidentialTypes() {
		tc, ok := confidentialCases[confidentialType]
		if !ok {
			unknown := confidentialType
			It("should reject unknown confidential type "+unknown, func(_ SpecContext) {
				Fail("unknown confidential type in E2E_CONFIDENTIAL_TYPES: " + unknown)
			})
			continue
		}
		It("should provision a "+tc.confidentialType+" Confidential VM node", func(ctx SpecContext) {
			runConfidentialTest(ctx, tc)
		}, SpecTimeout(15*time.Minute))
	}
})

func runConfidentialTest(ctx context.Context, tc confidentialCase) {
	prefix := environment.TestPrefix(karpv1.ArchitectureAmd64, karpv1.CapacityTypeOnDemand, "confidential", strings.ToLower(tc.confidentialType))
	suffix := environment.UniqueSuffix()
	name := prefix + "-" + suffix

	GinkgoWriter.Printf("[setup] confidential type=%s nodePool=%s\n", tc.confidentialType, name)

	var provisionedNodeName string
	DeferCleanup(func(ctx context.Context) {
		env.DeleteDeployment(ctx, name)
		env.DeleteNodePool(ctx, name)
		env.DeleteNodeClass(ctx, name)
		if provisionedNodeName != "" {
			Expect(env.WaitForNodeRemoval(ctx, provisionedNodeName)).To(Succeed())
		}
	})

	env.CreateNodeClassWithConfidentialType(ctx, name, tc.confidentialType)
	env.CreateNodePool(ctx, name, name, environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      tc.families,
		InstanceTypes: tc.instanceTypes,
	})
	env.CreateDeployment(ctx, name, name, name, karpv1.ArchitectureAmd64)

	pod := env.WaitForRunningPod(ctx, name)
	Expect(pod.Spec.NodeName).NotTo(BeEmpty())
	provisionedNodeName = pod.Spec.NodeName

	node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, provisionedNodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	Expect(node.Spec.ProviderID).NotTo(BeEmpty(), "node %s has no providerID", provisionedNodeName)

	instance, err := env.GetGCEInstance(ctx, node.Spec.ProviderID)
	Expect(err).NotTo(HaveOccurred(), "fetching GCE instance for node %s", provisionedNodeName)
	Expect(instance.ConfidentialInstanceConfig).NotTo(BeNil(),
		"GCE instance for node %s has no ConfidentialInstanceConfig", provisionedNodeName)
	Expect(instance.ConfidentialInstanceConfig.EnableConfidentialCompute).To(BeTrue(),
		"EnableConfidentialCompute should be true on node %s", provisionedNodeName)
	Expect(instance.ConfidentialInstanceConfig.ConfidentialInstanceType).To(Equal(tc.confidentialType),
		"unexpected confidential type on node %s", provisionedNodeName)
	Expect(instance.Scheduling).NotTo(BeNil(), "GCE instance for node %s has no Scheduling", provisionedNodeName)
	Expect(instance.Scheduling.OnHostMaintenance).To(Equal("TERMINATE"),
		"onHostMaintenance should be TERMINATE for confidential node %s", provisionedNodeName)
}

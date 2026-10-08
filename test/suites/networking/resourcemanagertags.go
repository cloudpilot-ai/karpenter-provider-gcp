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

package networking

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

var _ = Describe("Resource manager tags", Label("suite:networking"), func() {
	It("should bind spec.resourceManagerTags to the provisioned instance", func(ctx SpecContext) {
		Expect(env.ResourceManagerTagKey).NotTo(BeEmpty(), "set RESOURCE_MANAGER_TAG_KEY and run e2e-setup")

		tc := environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			InstanceTypes: []string{"n2-standard-2"},
		}
		name := environment.TestPrefix(tc.Arch, tc.CapacityType, "rmtags") + "-" + environment.UniqueSuffix()
		tagKey := env.ProjectID + "/" + env.ResourceManagerTagKey

		var provisionedNodeName string
		DeferCleanup(func(ctx context.Context) {
			env.DeleteDeployment(ctx, name)
			env.DeleteNodePool(ctx, name)
			env.DeleteNodeClass(ctx, name)
			if provisionedNodeName != "" {
				Expect(env.WaitForNodeRemoval(ctx, provisionedNodeName)).To(Succeed())
			}
		})

		env.CreateNodeClassWithResourceManagerTags(ctx, name, map[string]string{tagKey: environment.ResourceManagerTagValue})
		env.CreateNodePool(ctx, name, name, tc)
		env.CreateDeployment(ctx, name, name, name, tc.Arch)

		pod := env.WaitForRunningPod(ctx, name)
		provisionedNodeName = pod.Spec.NodeName
		node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, provisionedNodeName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		instance, err := env.GetGCEInstance(ctx, node.Spec.ProviderID)
		Expect(err).NotTo(HaveOccurred(), "fetching GCE instance for node %s", provisionedNodeName)

		// Resource Manager may report namespaced names with the project number instead of
		// the project ID, so match on the key and value short names.
		wantSuffix := "/" + env.ResourceManagerTagKey + "/" + environment.ResourceManagerTagValue
		Eventually(func(g Gomega) {
			tags, err := env.GetInstanceEffectiveTags(ctx, instance)
			g.Expect(err).NotTo(HaveOccurred())
			var bound []string
			for _, value := range tags {
				if strings.HasSuffix(value, wantSuffix) {
					bound = append(bound, value)
				}
			}
			g.Expect(bound).To(HaveLen(1), "instance %s effective tags %v missing %s", instance.Name, tags, wantSuffix)
		}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())
	}, SpecTimeout(15*time.Minute))
})

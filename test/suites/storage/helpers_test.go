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

package storage_test

import (
	"context"
	"sort"

	. "github.com/onsi/gomega"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func expectDiskTypeLabelsAndTopology(ctx context.Context, nodeName string, expected []string) {
	node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())

	for _, label := range expected {
		Expect(node.Labels).To(HaveKeyWithValue(label, "true"), "expected Node %s to have %s=true", nodeName, label)
	}

	Eventually(func(g Gomega) []string {
		csiNode, err := env.KubeClient.StorageV1().CSINodes().Get(ctx, nodeName, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		return topologyKeysForDriver(csiNode, "pd.csi.storage.gke.io")
	}, "2m", "5s").Should(ContainElements(expected), "expected PDCSI topology keys for Node %s", nodeName)
}

func topologyKeysForDriver(csiNode *storagev1.CSINode, driverName string) []string {
	for _, driver := range csiNode.Spec.Drivers {
		if driver.Name == driverName {
			keys := append([]string(nil), driver.TopologyKeys...)
			sort.Strings(keys)
			return keys
		}
	}
	return nil
}

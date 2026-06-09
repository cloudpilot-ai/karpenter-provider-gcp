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

package v1alpha1

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGCENodeClassCRDRejectsReservedMetadataKeys(t *testing.T) {
	crdPath := filepath.Join("..", "..", "..", "charts", "karpenter", "crds", "karpenter.k8s.gcp_gcenodeclasses.yaml")
	crd, err := os.ReadFile(crdPath)
	require.NoError(t, err)

	crdText := string(crd)
	require.Contains(t, crdText, `message: spec.metadata contains a reserved GKE metadata key`)
	require.Contains(t, crdText, `cluster-location`)

	reservedKeys := []string{
		"cluster-location",
		"cluster-name",
		"cluster-uid",
		"configure-sh",
		"containerd-configure-sh",
		"disable-address-manager",
		"enable-os-login",
		"gci-ensure-gke-docker",
		"gci-metrics-enabled",
		"gci-update-strategy",
		"instance-template",
		"install-ssh-psm1",
		"k8s-node-setup-psm1",
		"kube-env",
		"startup-script",
		"user-data",
		"user-profile-psm1",
		"windows-startup-script-ps1",
		"common-psm1",
		"kubeconfig",
		"kubelet-config",
	}

	for _, key := range reservedKeys {
		require.Contains(t, crdText, `'`+key+`'`, "reserved metadata key %q must be denied by the CRD", key)
	}
	require.NotContains(t, crdText, `'serial-port-logging-enable'`, "non-reserved metadata keys must remain freeform")
}

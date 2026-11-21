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

package metadata

import (
	"encoding/json"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	"sigs.k8s.io/yaml"
)

func TestBootstrapReleaseReplacesSourceArtifactsWithoutChangingContext(t *testing.T) {
	const installables = `'{"gke-node-internal-config":{"fixture":{"version":"v1.35.6-gke.0"}}}'`
	for _, test := range []struct{ name, oldArtifacts string }{
		{"missing source artifacts", ""},
		{"stale and malformed source artifacts", "KUBELET_VERSION: v1.35.8-gke.1796000\nSERVER_BINARY_TAR_URL: malformed-old-url\nSERVER_BINARY_TAR_HASH: old-hash\nKUBE_MANIFESTS_TAR_URL: old-manifests\nKUBE_MANIFESTS_TAR_HASH: old-manifests-hash\nSALT_TAR_URL: legacy-salt\nSALT_TAR_HASH: legacy-salt-hash\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := &compute.Metadata{Items: []*compute.MetadataItems{
				{Key: KubeEnvKey, Value: lo.ToPtr("CA_CERT: fixture-ca\nKUBERNETES_MASTER_NAME: example.invalid\nRENDERED_INSTALLABLES: " + installables + "\nCONTAINER_RUNTIME: containerd\n" + test.oldArtifacts)},
				{Key: KubeletConfigKey, Value: lo.ToPtr("featureGates:\n  RotateKubeletServerCertificate: true\n")},
				{Key: "configure-sh", Value: lo.ToPtr("old configure")},
				{Key: "user-data", Value: lo.ToPtr("old entrypoint")},
				{Key: "startup-script", Value: lo.ToPtr("competing startup")},
				{Key: "containerd-configure-sh", Value: lo.ToPtr("retained runtime policy")},
			}}
			before, err := json.Marshal(source)
			require.NoError(t, err)
			model, err := FromSourceTemplate(source)
			require.NoError(t, err)
			model.SetBootstrapRelease(BootstrapRelease{
				Version: "1.36.4-gke.2046003", ServerBinaryURL: "target-server", ServerBinaryHash: "target-server-sha512",
				ManifestsURL: "target-manifests", ManifestsHash: "target-manifests-sha512", ConfigureScript: "target configure", UserData: "target entrypoint",
			})
			rendered, err := model.ToComputeMetadata()
			require.NoError(t, err)
			values := metadataValuesFromCompute(rendered)
			var env map[string]string
			require.NoError(t, yaml.Unmarshal([]byte(values[KubeEnvKey]), &env))
			require.Equal(t, "v1.36.4-gke.2046003", env["KUBELET_VERSION"])
			require.Equal(t, "target-server", env["SERVER_BINARY_TAR_URL"])
			require.Equal(t, "target-server-sha512", env["SERVER_BINARY_TAR_HASH"])
			require.Equal(t, "target-manifests", env["KUBE_MANIFESTS_TAR_URL"])
			require.Equal(t, "target-manifests-sha512", env["KUBE_MANIFESTS_TAR_HASH"])
			require.NotContains(t, env, "SALT_TAR_URL")
			require.NotContains(t, env, "SALT_TAR_HASH")
			require.Equal(t, "fixture-ca", env["CA_CERT"])
			require.Equal(t, "example.invalid", env["KUBERNETES_MASTER_NAME"])
			retained, ok := model.GetKubeEnvEntry("RENDERED_INSTALLABLES")
			require.True(t, ok)
			require.Equal(t, installables, retained)
			require.True(t, model.kubeletConfig.FeatureGates["RotateKubeletServerCertificate"])
			require.Equal(t, "target configure", values["configure-sh"])
			require.Equal(t, "target entrypoint", values["user-data"])
			require.NotContains(t, values, "startup-script")
			require.Equal(t, "retained runtime policy", values["containerd-configure-sh"])
			after, err := json.Marshal(source)
			require.NoError(t, err)
			require.JSONEq(t, string(before), string(after), "source API object must not be mutated")
		})
	}
}

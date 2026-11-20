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

// BootstrapRelease is a complete set of core release artifacts supplied by the
// provider. It deliberately does not model inherited installables or cluster
// policy, and resolving its values does not belong in this package.
type BootstrapRelease struct {
	Version          string
	ServerBinaryURL  string
	ServerBinaryHash string
	ManifestsURL     string
	ManifestsHash    string
	ConfigureScript  string
	UserData         string
}

var releaseOwnedKubeEnvKeys = []string{
	"KUBELET_VERSION",
	"SERVER_BINARY_TAR_URL",
	"SERVER_BINARY_TAR_HASH",
	"KUBE_MANIFESTS_TAR_URL",
	"KUBE_MANIFESTS_TAR_HASH",
	"SALT_TAR_URL",
	"SALT_TAR_HASH",
}

// SetBootstrapRelease applies target-owned core artifacts without changing the
// retained installables, credentials, kubelet configuration or runtime policy.
func (m *InstanceMetadata) SetBootstrapRelease(release BootstrapRelease) {
	m.bootstrap = release
	m.SetKubeEnvEntry("KUBELET_VERSION", "v"+release.Version)
	m.SetKubeEnvEntry("SERVER_BINARY_TAR_URL", release.ServerBinaryURL)
	m.SetKubeEnvEntry("SERVER_BINARY_TAR_HASH", release.ServerBinaryHash)
	m.SetKubeEnvEntry("KUBE_MANIFESTS_TAR_URL", release.ManifestsURL)
	m.SetKubeEnvEntry("KUBE_MANIFESTS_TAR_HASH", release.ManifestsHash)
}

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

package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/metadata"
)

type sourceBootstrapResolver func(string, string) (BootstrapProfile, error)

func (f sourceBootstrapResolver) ResolveBootstrap(_ context.Context, manifests, server string) (BootstrapProfile, error) {
	return f(manifests, server)
}

func TestResolveBootstrapSource(t *testing.T) {
	kubeEnv := strings.Join([]string{
		`CA_CERT: "source-credential"`,
		`KUBE_MANIFESTS_TAR_URL: "https://storage.googleapis.com/gke-release/kubernetes/release/v1.35.6-gke.1250001/kubernetes-manifests.tar.gz"`,
		`KUBERNETES_MASTER_NAME: "master"`,
		`SERVER_BINARY_TAR_HASH: "hash"`,
		`SERVER_BINARY_TAR_URL: "https://storage.googleapis.com/gke-release/kubernetes/release/v1.35.6-gke.1250001/kubernetes-server-linux-amd64.tar.gz"`,
		`GVISOR_PLATFORM: "ptrace"`,
		`RENDERED_INSTALLABLES: '{"gvisor":{"gvisor":{"version":"old"}}}'`,
	}, "\n")
	source := &compute.Metadata{Items: []*compute.MetadataItems{{Key: metadata.KubeEnvKey, Value: &kubeEnv}}}
	called := false
	resolver := sourceBootstrapResolver(func(manifests, server string) (BootstrapProfile, error) {
		called = true
		require.Contains(t, manifests, "kubernetes-manifests.tar.gz")
		require.Contains(t, server, "kubernetes-server-linux-amd64.tar.gz")
		return BootstrapProfile{Release: "v1.35.6-gke.1250001"}, nil
	})
	profile, err := ResolveBootstrapSource(context.Background(), source, resolver)
	require.NoError(t, err)
	require.True(t, called)
	require.Equal(t, "v1.35.6-gke.1250001", profile.Release)

	malformed := strings.Replace(kubeEnv, `RENDERED_INSTALLABLES: '{"gvisor":{"gvisor":{"version":"old"}}}'`, `RENDERED_INSTALLABLES: '[]'`, 1)
	source.Items[0].Value = &malformed
	called = false
	_, err = ResolveBootstrapSource(context.Background(), source, resolver)
	require.ErrorContains(t, err, "RENDERED_INSTALLABLES")
	require.NotContains(t, err.Error(), "source-credential")
	require.False(t, called)

	missing := strings.Replace(kubeEnv, `SERVER_BINARY_TAR_HASH: "hash"`, "", 1)
	source.Items[0].Value = &missing
	_, err = ResolveBootstrapSource(context.Background(), source, resolver)
	require.ErrorContains(t, err, "SERVER_BINARY_TAR_HASH")
	require.NotContains(t, err.Error(), "source-credential")
	require.False(t, called)
}

func TestResolveBootstrapSourceTypedMetadataAndResolverErrors(t *testing.T) {
	kubeEnv := strings.Join([]string{
		`CA_CERT: "source-credential"`, `KUBE_MANIFESTS_TAR_URL: "manifest-url"`,
		`KUBERNETES_MASTER_NAME: "master"`, `SERVER_BINARY_TAR_HASH: "hash"`,
		`SERVER_BINARY_TAR_URL: "server-url"`,
	}, "\n")
	badConfig := "bad: ["
	source := &compute.Metadata{Items: []*compute.MetadataItems{
		{Key: metadata.KubeEnvKey, Value: &kubeEnv},
		{Key: metadata.KubeletConfigKey, Value: &badConfig},
	}}
	failure := &SandboxBootstrapUnsupported{Reason: "unknown consumers"}
	resolver := sourceBootstrapResolver(func(_, _ string) (BootstrapProfile, error) {
		return BootstrapProfile{}, failure
	})
	_, err := ResolveBootstrapSource(context.Background(), source, resolver)
	require.ErrorContains(t, err, "invalid typed configuration")
	require.NotContains(t, err.Error(), "source-credential")
	source.Items = source.Items[:1]
	_, err = ResolveBootstrapSource(context.Background(), source, resolver)
	require.True(t, errors.Is(err, failure))
}

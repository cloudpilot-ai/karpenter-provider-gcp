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

package instance

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/metadata"
)

func TestPatchKubeEnvServerBinaryForArch(t *testing.T) {
	serverBinaryHashCache.Clear()
	oldDefaultClient := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = oldDefaultClient })

	var requestedURL string
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requestedURL = req.URL.String()
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("arm64-sha512  kubernetes-server-linux-arm64.tar.gz")),
			Header:     http.Header{},
		}, nil
	})}

	target := metadata.NewInstanceMetadata()
	target.SetKubeEnvEntry("SERVER_BINARY_TAR_URL", "https://storage.googleapis.com/gke-release/kubernetes/release/v1.30.1-gke.123/kubernetes-server-linux-amd64.tar.gz")
	instanceType := &cloudprovider.InstanceType{Requirements: scheduling.NewRequirements(
		scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "arm64"),
	)}

	require.NoError(t, (&DefaultProvider{}).patchKubeEnvServerBinaryForArch(context.Background(), target, instanceType))

	url, ok := target.GetKubeEnvEntry("SERVER_BINARY_TAR_URL")
	require.True(t, ok)
	require.Equal(t, "https://storage.googleapis.com/gke-release/kubernetes/release/v1.30.1-gke.123/kubernetes-server-linux-arm64.tar.gz", url)
	hash, ok := target.GetKubeEnvEntry("SERVER_BINARY_TAR_HASH")
	require.True(t, ok)
	require.Equal(t, "arm64-sha512", hash)
	require.Equal(t, "https://storage.googleapis.com/gke-release/kubernetes/release/v1.30.1-gke.123/kubernetes-server-linux-arm64.tar.gz.sha512", requestedURL)
}

func TestPatchKubeEnvServerBinaryForArchNoopWhenArchMatches(t *testing.T) {
	target := metadata.NewInstanceMetadata()
	target.SetKubeEnvEntry("SERVER_BINARY_TAR_URL", "https://storage.googleapis.com/gke-release/kubernetes/release/v1.30.1-gke.123/kubernetes-server-linux-amd64.tar.gz")
	instanceType := &cloudprovider.InstanceType{Requirements: scheduling.NewRequirements(
		scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"),
	)}

	require.NoError(t, (&DefaultProvider{}).patchKubeEnvServerBinaryForArch(context.Background(), target, instanceType))

	_, ok := target.GetKubeEnvEntry("SERVER_BINARY_TAR_HASH")
	require.False(t, ok)
}

func TestGetServerBinaryHashCachesByVersionAndArch(t *testing.T) {
	serverBinaryHashCache.Clear()
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("cached-sha512  kubernetes-server-linux-arm64.tar.gz")),
			Header:     http.Header{},
		}, nil
	})}

	hash, err := getServerBinaryHash(context.Background(), "v1.30.1-gke.123", "arm64", client)
	require.NoError(t, err)
	require.Equal(t, "cached-sha512", hash)

	hash, err = getServerBinaryHash(context.Background(), "v1.30.1-gke.123", "arm64", client)
	require.NoError(t, err)
	require.Equal(t, "cached-sha512", hash)
	require.Equal(t, 1, requests)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

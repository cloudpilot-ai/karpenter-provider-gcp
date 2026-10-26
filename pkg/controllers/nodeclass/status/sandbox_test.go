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

package status

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/metadata"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/imagefamily"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/sandbox"
)

const completeSandboxSourceKubeEnv = "CA_CERT: synthetic-ca\n" +
	"KUBERNETES_MASTER_NAME: synthetic-master\n" +
	"SERVER_BINARY_TAR_HASH: synthetic-sha512\n" +
	"KUBE_MANIFESTS_TAR_URL: https://storage.googleapis.com/gke-release/kubernetes/release/v1.35.6-gke.1250001/kubernetes-manifests.tar.gz\n" +
	"SERVER_BINARY_TAR_URL: https://storage.googleapis.com/gke-release/kubernetes/release/v1.35.6-gke.1250001/kubernetes-server-linux-amd64.tar.gz\n"

type sourceTemplate struct{ metadata *compute.Metadata }

func (*sourceTemplate) Sync(context.Context) error               { panic("not used") }
func (*sourceTemplate) EnsureFallbackPool(context.Context) error { panic("must not write") }
func (s *sourceTemplate) GetSourceTemplateMetadata(context.Context) (*compute.Metadata, error) {
	return s.metadata, nil
}

type bootstrapResult struct{ err error }

func (r bootstrapResult) ResolveBootstrap(context.Context, string, string) (sandbox.BootstrapProfile, error) {
	return sandbox.BootstrapProfile{}, r.err
}

func TestSandboxReadinessIsEnabledOnlyAndFailClosed(t *testing.T) {
	nc := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{SandboxConfig: &v1alpha1.SandboxConfig{Type: "GVISOR"}, ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{{Family: v1alpha1.ImageFamilyContainerOptimizedOS, Channel: "cluster"}}}}
	reconciler := &Sandbox{sourceProvider: &sourceTemplate{metadata: &compute.Metadata{Items: []*compute.MetadataItems{{Key: metadata.KubeEnvKey, Value: lo.ToPtr(completeSandboxSourceKubeEnv)}}}}, resolver: bootstrapResult{err: &sandbox.SandboxBootstrapUnsupported{Reason: "unknown fingerprint"}}}
	_, err := reconciler.Reconcile(context.Background(), nc)
	require.NoError(t, err)
	require.True(t, nc.StatusConditions().Get(v1alpha1.ConditionTypeSandboxReady).IsFalse())
	nc.StatusConditions().SetTrue(v1alpha1.ConditionTypeImagesReady)
	require.False(t, nc.StatusConditions().Get("Ready").IsTrue())
	reconciler.resolver = bootstrapResult{}
	_, err = reconciler.Reconcile(context.Background(), nc)
	require.NoError(t, err)
	require.True(t, nc.StatusConditions().Get("Ready").IsTrue())
	// No RuntimeClass/client dependency: capability readiness works regardless of
	// admin-owned class names. Ordinary Ubuntu does not access the source at all.
	nc.Spec.SandboxConfig = nil
	nc.Spec.ImageSelectorTerms = []v1alpha1.ImageSelectorTerm{{Family: v1alpha1.ImageFamilyUbuntu}}
	reconciler = &Sandbox{}
	_, err = reconciler.Reconcile(context.Background(), nc)
	require.NoError(t, err)
	require.True(t, nc.StatusConditions().Get("Ready").IsTrue())
	require.Nil(t, nc.StatusConditions().Get(v1alpha1.ConditionTypeSandboxReady))
}

func TestSandboxReadinessRejectsMalformedSourceAndInvalidSelectors(t *testing.T) {
	nc := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{SandboxConfig: &v1alpha1.SandboxConfig{Type: "GVISOR"}, ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{{Family: v1alpha1.ImageFamilyContainerOptimizedOS, Channel: "cluster"}}}}
	s := &Sandbox{sourceProvider: &sourceTemplate{metadata: &compute.Metadata{Items: []*compute.MetadataItems{{Key: metadata.KubeEnvKey, Value: lo.ToPtr(completeSandboxSourceKubeEnv + "RENDERED_INSTALLABLES: {bad")}}}}, resolver: bootstrapResult{}}
	_, err := s.Reconcile(context.Background(), nc)
	require.NoError(t, err)
	require.True(t, nc.StatusConditions().Get(v1alpha1.ConditionTypeSandboxReady).IsFalse())
	require.Contains(t, nc.StatusConditions().Get(v1alpha1.ConditionTypeSandboxReady).Message, "RENDERED_INSTALLABLES")
	nc.Spec.ImageSelectorTerms = append(nc.Spec.ImageSelectorTerms, v1alpha1.ImageSelectorTerm{Family: v1alpha1.ImageFamilyUbuntu})
	var configError *sandbox.ConfigurationError
	require.ErrorAs(t, s.validate(context.Background(), nc), &configError)
}

type resolvedImages imagefamily.Images

func (i resolvedImages) List(context.Context, *v1alpha1.GCENodeClass) (imagefamily.Images, error) {
	return imagefamily.Images(i), nil
}
func TestImageStatusCopiesInstallerIdentity(t *testing.T) {
	installer := &v1alpha1.SandboxInstaller{Version: "20260323.0_RC03", Digest: "sha256:" + strings.Repeat("a", 64)}
	reconciler := &Image{imageProvider: resolvedImages{{SourceImage: "selected-public-image", SandboxInstaller: installer, Requirements: scheduling.NewRequirements(scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "arm64"))}}}
	nc := &v1alpha1.GCENodeClass{}
	_, err := reconciler.Reconcile(context.Background(), nc)
	require.NoError(t, err)
	require.Equal(t, installer, nc.Status.Images[0].SandboxInstaller)
	installer.Digest = "changed-outside-status"
	require.Equal(t, "sha256:"+strings.Repeat("a", 64), nc.Status.Images[0].SandboxInstaller.Digest)
}

type unavailableSandboxImages struct{}

func (unavailableSandboxImages) List(context.Context, *v1alpha1.GCENodeClass) (imagefamily.Images, error) {
	return nil, fmt.Errorf("public installer unavailable")
}
func TestSandboxImageArtifactFailureClearsReadiness(t *testing.T) {
	nc := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{SandboxConfig: &v1alpha1.SandboxConfig{Type: "GVISOR"}}}
	nc.StatusConditions().SetTrue(v1alpha1.ConditionTypeImagesReady)
	reconciler := &Image{imageProvider: unavailableSandboxImages{}}
	_, err := reconciler.Reconcile(context.Background(), nc)
	require.NoError(t, err)
	require.True(t, nc.StatusConditions().Get(v1alpha1.ConditionTypeImagesReady).IsFalse())
	nc.Spec.SandboxConfig = nil
	_, err = reconciler.Reconcile(context.Background(), nc)
	require.Error(t, err) // ordinary transient behavior unchanged
}

func TestSandboxReadinessRejectsRequiredSourceFieldsMissingAtLaunch(t *testing.T) {
	for _, field := range []string{"CA_CERT: synthetic-ca\n", "KUBERNETES_MASTER_NAME: synthetic-master\n", "SERVER_BINARY_TAR_HASH: synthetic-sha512\n"} {
		nc := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{SandboxConfig: &v1alpha1.SandboxConfig{Type: "GVISOR"}}}
		source := strings.Replace(completeSandboxSourceKubeEnv, field, "", 1)
		s := &Sandbox{sourceProvider: &sourceTemplate{metadata: &compute.Metadata{Items: []*compute.MetadataItems{{Key: metadata.KubeEnvKey, Value: lo.ToPtr(source)}}}}, resolver: bootstrapResult{}}
		_, err := s.Reconcile(context.Background(), nc)
		require.NoError(t, err)
		condition := nc.StatusConditions().Get(v1alpha1.ConditionTypeSandboxReady)
		require.True(t, condition.IsFalse())
		require.Contains(t, condition.Message, strings.SplitN(field, ":", 2)[0])
	}
}

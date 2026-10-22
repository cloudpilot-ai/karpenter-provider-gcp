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
	"time"

	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/metadata"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/nodepooltemplate"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/sandbox"
)

// Sandbox readiness is node/bootstrap capability, not workload RuntimeClass
// policy. It never reads or owns RuntimeClasses (including custom names).
type Sandbox struct {
	sourceProvider nodepooltemplate.Provider
	resolver       sandbox.BootstrapResolver
}

func (s *Sandbox) Reconcile(ctx context.Context, nodeClass *v1alpha1.GCENodeClass) (reconcile.Result, error) {
	if !nodeClass.SandboxEnabled() {
		// Remove stale capability status without adding an ordinary/Ubuntu gate.
		return reconcile.Result{}, nodeClass.StatusConditions().Clear(v1alpha1.ConditionTypeSandboxReady)
	}
	err := s.validate(ctx, nodeClass)
	if err != nil {
		// Configuration failures never enter unavailable offerings. Do not include
		// raw source values in the condition; errors only describe owned structure.
		message := err.Error()
		if len(message) > 256 {
			message = message[:256]
		}
		nodeClass.StatusConditions().SetFalse(v1alpha1.ConditionTypeSandboxReady, "SandboxBootstrapUnsupported", message)
		return reconcile.Result{RequeueAfter: time.Minute}, nil
	}
	nodeClass.StatusConditions().SetTrue(v1alpha1.ConditionTypeSandboxReady)
	return reconcile.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (s *Sandbox) validate(ctx context.Context, nodeClass *v1alpha1.GCENodeClass) error {
	if err := sandbox.ValidateConfiguration(nodeClass); err != nil {
		return err
	}
	source, err := s.sourceProvider.GetSourceTemplateMetadata(ctx)
	if err != nil {
		return &sandbox.ConfigurationError{Reason: "bootstrap source template is unavailable"}
	}
	target, err := metadata.FromSourceTemplate(source)
	if err != nil {
		return &sandbox.ConfigurationError{Reason: "bootstrap source metadata has an invalid typed configuration"}
	}
	if err := metadata.ValidateSourceKubeEnv(target); err != nil {
		return &sandbox.ConfigurationError{Reason: err.Error()}
	}
	if err := target.ApplySandbox(nil); err != nil {
		return &sandbox.ConfigurationError{Reason: err.Error()}
	}
	manifests, _ := target.GetKubeEnvEntry("KUBE_MANIFESTS_TAR_URL")
	server, _ := target.GetKubeEnvEntry("SERVER_BINARY_TAR_URL")
	_, err = s.resolver.ResolveBootstrap(ctx, manifests, server)
	return err
}

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

	"google.golang.org/api/compute/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/metadata"
)

// ResolveBootstrapSource checks the same effective ordinary source metadata at
// readiness, candidate selection and adoption. Never include kube-env values
// in configuration errors: the source also contains bootstrap credentials.
func ResolveBootstrapSource(ctx context.Context, source *compute.Metadata, resolver BootstrapResolver) (BootstrapProfile, error) {
	target, err := metadata.FromSourceTemplate(source)
	if err != nil {
		return BootstrapProfile{}, &ConfigurationError{Reason: "bootstrap source metadata has an invalid typed configuration"}
	}
	if err := metadata.ValidateSourceKubeEnv(target); err != nil {
		return BootstrapProfile{}, &ConfigurationError{Reason: err.Error()}
	}
	if err := target.ApplySandbox(nil); err != nil {
		return BootstrapProfile{}, &ConfigurationError{Reason: err.Error()}
	}
	manifests, _ := target.GetKubeEnvEntry("KUBE_MANIFESTS_TAR_URL")
	server, _ := target.GetKubeEnvEntry("SERVER_BINARY_TAR_URL")
	return resolver.ResolveBootstrap(ctx, manifests, server)
}

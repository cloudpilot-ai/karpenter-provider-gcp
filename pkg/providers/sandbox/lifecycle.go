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
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/metadata"
)

// LaunchIdentity is persisted only at launch, never synthesized for inventory.
func LaunchIdentity(class *v1alpha1.GCENodeClass, installer v1alpha1.SandboxInstaller, profile BootstrapProfile, machineType, arch string) (metadata.SandboxIdentity, error) {
	if class.UID == "" {
		return metadata.SandboxIdentity{}, &ConfigurationError{Reason: "launch GCENodeClass has no UID"}
	}
	hardware, err := ResolveHardwarePolicy(machineType, arch)
	if err != nil {
		return metadata.SandboxIdentity{}, err
	}
	return metadata.SandboxIdentity{
		Runtime:   v1alpha1.SandboxRuntimeGVISOR,
		Installer: InstallerReference(installer),
		Profile:   profile.Identity(hardware.Variant),
		ClassUID:  string(class.UID),
		ClassHash: v1alpha1.GCENodeClassHashVersion + "/" + class.Hash(),
	}, nil
}

// NodePoolOwnerUID accepts the upstream non-controller owner reference shape.
// A label is not UID proof. Conflicting or malformed NodePool owners fail closed.
func NodePoolOwnerUID(claim *karpv1.NodeClaim) (string, error) {
	uid := ""
	for _, owner := range claim.OwnerReferences {
		if owner.Kind != "NodePool" {
			continue
		}
		gv, err := schema.ParseGroupVersion(owner.APIVersion)
		if err != nil || gv.Group != "karpenter.sh" || gv.Version == "" || owner.Name != claim.Labels[karpv1.NodePoolLabelKey] || owner.UID == "" {
			return "", &ConfigurationError{Reason: "invalid NodePool owner identity"}
		}
		if uid != "" {
			return "", &ConfigurationError{Reason: "ambiguous NodePool owner identity"}
		}
		uid = string(owner.UID)
	}
	return uid, nil
}

func ValidateIdentityMode(identity *metadata.SandboxIdentity, labels map[string]string, enabled bool) error {
	label, labeled := labels[metadata.SandboxRuntimeInstanceLabel]
	if !enabled {
		if identity != nil || labeled {
			return &ConfigurationError{Reason: "ordinary request cannot adopt a VM with sandbox identity"}
		}
		return nil
	}
	if identity == nil || !labeled || !validRuntimeIdentity(identity, label) {
		return &ConfigurationError{Reason: "VM sandbox runtime metadata and label do not agree"}
	}
	if identity.Installer == "" || identity.Profile == "" || identity.ClassUID == "" || identity.ClassHash == "" {
		return &ConfigurationError{Reason: "VM sandbox identity is incomplete"}
	}
	return nil
}

func ValidateLaunchBinding(actual *metadata.SandboxIdentity, class *v1alpha1.GCENodeClass, claim *karpv1.NodeClaim) error {
	if actual.ClassUID != string(class.UID) || actual.ClassHash != v1alpha1.GCENodeClassHashVersion+"/"+class.Hash() {
		return &ConfigurationError{Reason: "VM launch GCENodeClass identity changed"}
	}
	uid, err := NodePoolOwnerUID(claim)
	if err != nil {
		return err
	}
	if uid != actual.PoolUID {
		return &ConfigurationError{Reason: "VM launch NodePool owner identity changed"}
	}
	if claim.Spec.NodeClassRef == nil || claim.Spec.NodeClassRef.Name != class.Name || claim.Spec.NodeClassRef.Group != apis.Group || claim.Spec.NodeClassRef.Kind != "GCENodeClass" {
		return &ConfigurationError{Reason: fmt.Sprintf("VM launch reference does not match GCENodeClass %s", class.Name)}
	}
	return nil
}

func validRuntimeIdentity(identity *metadata.SandboxIdentity, label string) bool {
	return !identity.Malformed && identity.Runtime == v1alpha1.SandboxRuntimeGVISOR && label == identity.Runtime
}

// ImageMatches compares a public resource ID with its canonical Compute URL.
// Suffix matching would also accept foreign hosts or resource-name prefixes.
func ImageMatches(actual, source string) bool {
	return actual != "" && source != "" && canonicalImageID(actual) == canonicalImageID(source)
}

func canonicalImageID(image string) string {
	for _, prefix := range []string{"https://www.googleapis.com/compute/v1/", "https://compute.googleapis.com/compute/v1/"} {
		if strings.HasPrefix(image, prefix) {
			return strings.TrimPrefix(image, prefix)
		}
	}
	return image
}

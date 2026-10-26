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
	"encoding/json"
	"fmt"
	"mime"
	"regexp"
	"strings"
	"time"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

var labelPattern = regexp.MustCompile(`^([0-9]{8})-([0-9]+)_rc([0-9]{2})$`)
var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

const (
	ociIndex         = "application/vnd.oci.image.index.v1+json"
	dockerIndex      = "application/vnd.docker.distribution.manifest.list.v2+json"
	ociManifest      = "application/vnd.oci.image.manifest.v1+json"
	dockerManifest   = "application/vnd.docker.distribution.manifest.v2+json"
	maxManifestBytes = 1024 * 1024
)

// InstallerVersion recognizes only the observed GKE image-label schema. Unknown
// schemas must be reviewed, not guessed into a mutable or fallback tag.
func InstallerVersion(label string) (string, error) {
	m := labelPattern.FindStringSubmatch(label)
	if m == nil {
		return "", fmt.Errorf("missing or unsupported gvisor-version label %q", label)
	}
	if _, err := time.Parse("20060102", m[1]); err != nil {
		return "", fmt.Errorf("invalid date in gvisor-version label %q", label)
	}
	return m[1] + "." + m[2] + "_RC" + m[3], nil
}

// InstallerReference returns the immutable public reference for native installables.
func InstallerReference(installer v1alpha1.SandboxInstaller) string {
	return InstallerRepository + "@" + installer.Digest
}

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
	} `json:"platform"`
}

type installerManifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Manifests     []descriptor `json:"manifests"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
}

func validDescriptor(d descriptor) bool { return digestPattern.MatchString(d.Digest) && d.Size > 0 }
func isManifest(mt string) bool         { return mt == ociManifest || mt == dockerManifest }

// ResolveInstaller binds a selected image's label and architecture to a verified
// public OCI identity. Successful results refresh within five minutes.
func (r *Resolver) ResolveInstaller(ctx context.Context, label, arch string) (*v1alpha1.SandboxInstaller, error) {
	version, err := InstallerVersion(label)
	if err != nil {
		return nil, err
	}
	if arch != "amd64" && arch != "arm64" {
		return nil, fmt.Errorf("unsupported sandbox architecture %q", arch)
	}
	key := "installer:" + version + ":" + arch
	if v, ok := r.get(key); ok {
		copy := v.(v1alpha1.SandboxInstaller)
		return &copy, nil
	}
	return r.resolveInstaller(ctx, version, arch, key)
}

func (r *Resolver) resolveInstaller(ctx context.Context, version, arch, key string) (*v1alpha1.SandboxInstaller, error) {
	base := "https://gke.gcr.io/v2/gke-gvisor-installer/"
	b, h, err := r.fetch(ctx, base+"manifests/"+version, strings.Join([]string{ociIndex, dockerIndex, ociManifest, dockerManifest}, ","), maxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("resolving gVisor installer %s: %w", version, err)
	}
	digest := digestBytes(b)
	if !digestPattern.MatchString(h.Get("Docker-Content-Digest")) || h.Get("Docker-Content-Digest") != digest {
		return nil, fmt.Errorf("installer %s manifest content digest is absent or mismatched", version)
	}
	var doc installerManifest
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("decoding installer manifest: %w", err)
	}
	mt, _, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil || mt != doc.MediaType || doc.SchemaVersion != 2 {
		return nil, fmt.Errorf("unsupported installer schema or media type")
	}
	if err := r.validateInstallerPlatform(ctx, doc, arch); err != nil {
		return nil, fmt.Errorf("installer %s: %w", version, err)
	}

	result := v1alpha1.SandboxInstaller{Version: version, Digest: digest}
	r.put(key, result)
	return &result, nil
}

func (r *Resolver) validateInstallerPlatform(ctx context.Context, doc installerManifest, arch string) error {
	switch doc.MediaType {
	case ociIndex, dockerIndex:
		return validateIndexPlatform(doc.Manifests, arch)
	case ociManifest, dockerManifest:
		return r.validateManifestPlatform(ctx, doc, arch)
	default:
		return fmt.Errorf("unsupported installer media type %q", doc.MediaType)
	}
}

func validateIndexPlatform(manifests []descriptor, arch string) error {
	found := false
	for _, d := range manifests {
		if !validDescriptor(d) || !isManifest(d.MediaType) || d.Platform == nil {
			return fmt.Errorf("invalid installer platform descriptor")
		}
		if d.Platform.OS == "linux" && d.Platform.Architecture == arch && supportedVariant(arch, d.Platform.Variant) {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("no supported linux/%s platform", arch)
	}
	return nil
}
func supportedVariant(arch, variant string) bool {
	return variant == "" || (arch == "arm64" && variant == "v8")
}

func (r *Resolver) validateManifestPlatform(ctx context.Context, doc installerManifest, arch string) error {
	if err := validateManifestDescriptors(doc); err != nil {
		return err
	}
	config, _, err := r.fetch(ctx, "https://gke.gcr.io/v2/gke-gvisor-installer/blobs/"+doc.Config.Digest, "application/json", maxManifestBytes)
	if err != nil {
		return err
	}
	if digestBytes(config) != doc.Config.Digest || int64(len(config)) != doc.Config.Size {
		return fmt.Errorf("installer config content digest/size mismatch")
	}
	var platform struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
	}
	if json.Unmarshal(config, &platform) != nil || platform.OS != "linux" || platform.Architecture != arch || !supportedVariant(arch, platform.Variant) {
		return fmt.Errorf("installer config does not support linux/%s", arch)
	}
	return nil
}

func validateManifestDescriptors(doc installerManifest) error {
	if !validDescriptor(doc.Config) || (doc.Config.MediaType != "application/vnd.oci.image.config.v1+json" && doc.Config.MediaType != "application/vnd.docker.container.image.v1+json") || len(doc.Layers) == 0 {
		return fmt.Errorf("invalid installer config/layers")
	}
	for _, d := range doc.Layers {
		if !validDescriptor(d) || !isLayerMediaType(d.MediaType) {
			return fmt.Errorf("invalid installer layer")
		}
	}
	return nil
}
func isLayerMediaType(mt string) bool {
	return mt == "application/vnd.oci.image.layer.v1.tar+gzip" || mt == "application/vnd.docker.image.rootfs.diff.tar.gzip" || mt == "application/vnd.oci.image.layer.v1.tar"
}

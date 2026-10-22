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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/metadata"
)

const (
	maxCompressedBytes   = 2 * 1024 * 1024
	maxDecompressedBytes = 16 * 1024 * 1024
	maxMemberBytes       = 2 * 1024 * 1024
)

// SandboxBootstrapUnsupported is a fail-closed consumer compatibility failure.
// Callers must surface it as configuration/readiness failure, not unavailable
// capacity. It must prevent launch without hiding historical instances.
type SandboxBootstrapUnsupported struct{ Reason string }

func (e *SandboxBootstrapUnsupported) Error() string {
	return "SandboxBootstrapUnsupported: " + e.Reason
}
func IsBootstrapUnsupported(err error) bool {
	var target *SandboxBootstrapUnsupported
	return errors.As(err, &target)
}

// BootstrapProfile identifies reviewed consumers and native runtime policy.
// Matching consumers does not discover control-plane hardware/security policy.
// Hardware-specific policy is applied later and must be part of VM identity.
type BootstrapProfile struct {
	Release       string
	ArchiveDigest string
	Schema        string
	Policy        string
}

// Identity includes the caller's independently validated hardware policy variant.
// Persist this full identity on the VM; do not infer it from today's NodeClass.
func (p BootstrapProfile) Identity(hardwarePolicy string) string {
	return p.Schema + "/" + p.Policy + "/" + hardwarePolicy
}

type bootstrapFingerprint struct {
	Source        string            `json:"source"`
	ArchiveSHA256 string            `json:"archiveSHA256"`
	Schema        string            `json:"schema"`
	Policy        string            `json:"policy"`
	Members       map[string]string `json:"members"`
}

//go:embed testdata/bootstrap-profile.json
var acceptedBootstrapJSON []byte

var acceptedBootstrap = func() bootstrapFingerprint {
	var p bootstrapFingerprint
	if err := json.Unmarshal(acceptedBootstrapJSON, &p); err != nil {
		panic(err)
	}
	return p
}()
var releasePath = regexp.MustCompile(`^https://storage[.]googleapis[.]com/(gke-release(?:-asia|-eu)?)/kubernetes/release/(v[0-9]+[.][0-9]+[.][0-9]+-gke[.][0-9]+)/(kubernetes-manifests[.]tar[.]gz|kubernetes-server-linux-(amd64|arm64)[.]tar[.]gz)$`)

// ResolveBootstrap validates the effective source URLs, not the master version.
// URLs must name the same versioned public GKE release. No credentials, scripts,
// or private kube-env are retained; all hashing is performed in memory.
func (r *Resolver) ResolveBootstrap(ctx context.Context, manifestsURL, serverBinaryURL string) (BootstrapProfile, error) {
	manifestURL, release, err := ValidateBootstrapURLs(manifestsURL, serverBinaryURL)
	if err != nil {
		return BootstrapProfile{}, err
	}
	key := "bootstrap:" + manifestURL + ":" + acceptedBootstrap.Schema + ":" + acceptedBootstrap.Policy
	if v, ok := r.get(key); ok {
		return v.(BootstrapProfile), nil
	}
	b, _, err := r.fetch(ctx, manifestURL, "application/gzip", maxCompressedBytes)
	if err != nil {
		return BootstrapProfile{}, &SandboxBootstrapUnsupported{Reason: fmt.Sprintf("fetching public manifests for %s: %v", release, err)}
	}
	if err := matchBootstrapArchive(b); err != nil {
		return BootstrapProfile{}, &SandboxBootstrapUnsupported{Reason: fmt.Sprintf("release %s: %v; a reviewed controller compatibility-profile update is required", release, err)}
	}
	p := BootstrapProfile{Release: release, ArchiveDigest: digestBytes(b), Schema: acceptedBootstrap.Schema, Policy: acceptedBootstrap.Policy}
	r.put(key, p)
	return p, nil
}

func matchBootstrapArchive(b []byte) error {
	if len(b) > maxCompressedBytes {
		return fmt.Errorf("compressed manifests exceed 2 MiB")
	}
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer gz.Close()
	// Read through gzip EOF before parsing so trailing bytes and checksum errors
	// cannot bypass the total decompression limit at tar's end marker.
	expanded, err := io.ReadAll(io.LimitReader(gz, maxDecompressedBytes+1))
	if err != nil {
		return err
	}
	if len(expanded) > maxDecompressedBytes {
		return fmt.Errorf("decompressed manifests exceed 16 MiB")
	}
	return matchBootstrapMembers(expanded)
}

func matchBootstrapMembers(expanded []byte) error {
	tr := tar.NewReader(bytes.NewReader(expanded))
	seen := map[string]bool{}
	entries := map[string]byte{}
	directories := map[string]bool{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if h.Size > maxMemberBytes {
			return fmt.Errorf("archive member exceeds 2 MiB")
		}
		if err := validateBootstrapEntry(h, entries, directories); err != nil {
			return err
		}
		want, required := acceptedBootstrap.Members[h.Name]
		if !required {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return fmt.Errorf("non-regular bootstrap consumer %s", h.Name)
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		if strings.TrimPrefix(digestBytes(content), "sha256:") != want {
			return fmt.Errorf("unknown bootstrap consumer fingerprint: %s", h.Name)
		}
		seen[h.Name] = true
	}
	if len(seen) != len(acceptedBootstrap.Members) {
		return fmt.Errorf("missing required full-path bootstrap consumers")
	}
	return nil
}

// validateMirrors accepts GKE's YAML scalar containing ordered comma-separated
// mirrors. Every entry is validated before the first trusted mirror is fetched.
func validateMirrors(raw string, manifests bool) (string, string, error) {
	scalar, err := metadata.DecodeKubeEnvScalar(raw)
	if err != nil {
		return "", "", &SandboxBootstrapUnsupported{Reason: "artifact URL must be a YAML string scalar"}
	}
	var effective, release, artifact string
	for _, entry := range strings.Split(scalar, ",") {
		entry = strings.TrimSpace(entry)
		entryRelease, entryArtifact, err := validateMirror(entry, manifests)
		if err != nil {
			return "", "", err
		}
		if artifact != "" && artifact != entryArtifact {
			return "", "", &SandboxBootstrapUnsupported{Reason: "artifact mirrors name different artifacts or architectures"}
		}
		artifact = entryArtifact
		if release != "" && release != entryRelease {
			return "", "", &SandboxBootstrapUnsupported{Reason: "artifact mirrors name different releases"}
		}
		if effective == "" {
			effective = entry
		}
		release = entryRelease
	}
	return effective, release, nil
}

// ValidateBootstrapURLs validates every source mirror before any architecture
// rewriting, so replacement cannot conceal inconsistent source artifacts.
func ValidateBootstrapURLs(manifests, server string) (string, string, error) {
	effective, release, err := validateMirrors(manifests, true)
	if err != nil {
		return "", "", err
	}
	_, serverRelease, err := validateMirrors(server, false)
	if err != nil {
		return "", "", err
	}
	if release != serverRelease {
		return "", "", &SandboxBootstrapUnsupported{Reason: "effective manifests and server binaries name different GKE releases"}
	}
	return effective, release, nil
}

func validateMirror(entry string, manifests bool) (string, string, error) {
	m := releasePath.FindStringSubmatch(entry)
	if m == nil {
		return "", "", &SandboxBootstrapUnsupported{Reason: "every artifact mirror must name an exact versioned public GKE artifact in a known release bucket on storage.googleapis.com"}
	}
	if (manifests && m[3] != "kubernetes-manifests.tar.gz") || (!manifests && m[4] == "") {
		return "", "", &SandboxBootstrapUnsupported{Reason: "artifact mirror names the wrong artifact type"}
	}
	return m[2], m[3], nil
}

// Only canonical regular files and directories are accepted, even for members
// outside the fingerprint set. Otherwise extraction aliases or ancestor links
// could replace the reviewed consumers after their hashes were validated.
func validateBootstrapEntry(h *tar.Header, entries map[string]byte, directories map[string]bool) error {
	if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
		return fmt.Errorf("non-regular/non-directory bootstrap archive member")
	}
	name, err := canonicalBootstrapPath(h)
	if err != nil {
		return err
	}
	if _, exists := entries[name]; exists {
		return fmt.Errorf("duplicate bootstrap archive path")
	}
	if h.Typeflag == tar.TypeReg && directories[name] {
		return fmt.Errorf("bootstrap archive file replaces a directory ancestor")
	}
	for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
		if entries[parent] == tar.TypeReg {
			return fmt.Errorf("bootstrap archive has a non-directory ancestor")
		}
		directories[parent] = true
	}
	entries[name] = h.Typeflag
	return nil
}

func canonicalBootstrapPath(h *tar.Header) (string, error) {
	name := h.Name
	if h.Typeflag == tar.TypeDir {
		name = strings.TrimSuffix(name, "/")
	}
	if name == "." || name == ".." || strings.HasPrefix(name, "../") || path.IsAbs(name) || path.Clean(name) != name || strings.Contains(name, `\`) {
		return "", fmt.Errorf("noncanonical bootstrap archive member path")
	}
	return name, nil
}

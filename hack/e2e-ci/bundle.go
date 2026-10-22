/*
Copyright 2025 The CloudPilot AI Authors.

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

package e2eci

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"debug/buildinfo"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	MaxBundleBytes    int64 = 2 << 30
	MaxFileBytes      int64 = 1 << 30
	MaxMetadataBytes  int64 = 64 << 10
	MaxArchiveEntries       = 20000
	ManifestName            = "manifest.json"
	ControllerArchive       = "controller.oci.tar"
	ControllerChart         = "karpenter.tgz"
	CRDChart                = "karpenter-crd.tgz"
	TestBinary              = "e2e.test"
	GinkgoBinary            = "ginkgo"
)

var bundleFiles = []string{ControllerArchive, ControllerChart, CRDChart, TestBinary, GinkgoBinary}
var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var versionPattern = regexp.MustCompile(`^v2\.[0-9]+\.[0-9]+$`)

type FileDigest struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest is intentionally data only. No argv, deployment targets, templates or trusted tools come from a build.
type Manifest struct {
	Schema        int          `json:"schema"`
	Source        Frozen       `json:"source"`
	Platform      string       `json:"platform"`
	GinkgoVersion string       `json:"ginkgo_version"`
	Outcome       string       `json:"outcome"`
	Files         []FileDigest `json:"files"`
}
type Bundle struct {
	Root     string
	Manifest Manifest
}

func validPath(name string) bool {
	return name != "" && name != "." && !strings.ContainsAny(name, "\\\x00:") && !path.IsAbs(name) && path.Clean(name) == name && name != ".." && !strings.HasPrefix(name, "../")
}
func fileDigest(file, name string) (FileDigest, error) {
	f, err := os.Open(file)
	if err != nil {
		return FileDigest{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return FileDigest{}, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > MaxFileBytes {
		return FileDigest{}, fmt.Errorf("invalid bundle file %s", name)
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return FileDigest{}, err
	}
	if n != info.Size() {
		return FileDigest{}, errors.New("bundle file changed while reading")
	}
	return FileDigest{Name: name, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// VerifyBundle extracts into a fresh private directory and removes it on all failures.
// It must run before credentials are obtained. Hashes attest transfer, not honest PR code.
func VerifyBundle(r io.Reader, parent string, expected Frozen) (_ *Bundle, retErr error) {
	if err := expected.Validate(); err != nil {
		return nil, err
	}
	root, err := os.MkdirTemp(parent, "verified-bundle-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			os.RemoveAll(root)
		}
	}()
	limited := &io.LimitedReader{R: r, N: MaxBundleBytes + 1}
	tr := tar.NewReader(limited)
	seen := map[string]bool{}
	var manifest Manifest
	var total int64
	allowed := map[string]bool{ManifestName: true}
	for _, name := range bundleFiles {
		allowed[name] = true
	}
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if !validPath(header.Name) || !allowed[header.Name] || seen[header.Name] || header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > MaxFileBytes || len(header.PAXRecords) != 0 || header.Linkname != "" {
			return nil, fmt.Errorf("unsafe bundle entry %q", header.Name)
		}
		total += header.Size
		if total > MaxBundleBytes {
			return nil, errors.New("bundle too large")
		}
		seen[header.Name] = true
		if header.Name == ManifestName {
			if err := DecodeStrict(tr, MaxMetadataBytes, &manifest); err != nil {
				return nil, err
			}
			continue
		}
		f, err := os.OpenFile(filepath.Join(root, header.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		_, copyErr := io.Copy(f, tr)
		closeErr := f.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return nil, err
		}
	}
	// The archive cannot smuggle another archive after tar's end marker.
	trailing, err := io.ReadAll(io.LimitReader(limited, 1025))
	if err != nil {
		return nil, err
	}
	if len(trailing) != 0 || limited.N <= 0 {
		return nil, errors.New("trailing or oversized bundle")
	}
	if len(seen) != len(allowed) || manifest.Schema != 1 || manifest.Source != expected || manifest.Platform != "linux/amd64" || !versionPattern.MatchString(manifest.GinkgoVersion) || manifest.Outcome != "success" || len(manifest.Files) != len(bundleFiles) {
		return nil, errors.New("incomplete bundle or source/platform/version mismatch")
	}
	checked := map[string]bool{}
	for _, entry := range manifest.Files {
		if !allowed[entry.Name] || entry.Name == ManifestName || checked[entry.Name] {
			return nil, errors.New("unexpected manifest file")
		}
		checked[entry.Name] = true
		actual, err := fileDigest(filepath.Join(root, entry.Name), entry.Name)
		if err != nil {
			return nil, err
		}
		if actual != entry {
			return nil, fmt.Errorf("digest/length mismatch for %s", entry.Name)
		}
	}
	if err := validateContents(root, manifest); err != nil {
		return nil, err
	}
	for _, name := range []string{TestBinary, GinkgoBinary} {
		if err := os.Chmod(filepath.Join(root, name), 0700); err != nil {
			return nil, err
		}
	}
	return &Bundle{Root: root, Manifest: manifest}, nil
}

func validateContents(root string, m Manifest) error {
	if err := validateOCI(filepath.Join(root, ControllerArchive), m.Source.HeadSHA); err != nil {
		return fmt.Errorf("OCI: %w", err)
	}
	for _, chart := range []struct{ file, root string }{{ControllerChart, "karpenter"}, {CRDChart, "karpenter-crd"}} {
		if err := validateChart(filepath.Join(root, chart.file), chart.root); err != nil {
			return fmt.Errorf("chart: %w", err)
		}
	}
	for _, name := range []string{TestBinary, GinkgoBinary} {
		file := filepath.Join(root, name)
		executable, err := elf.Open(file)
		if err != nil {
			return err
		}
		valid := executable.Machine == elf.EM_X86_64 && executable.Class == elf.ELFCLASS64
		executable.Close()
		if !valid {
			return errors.New("prepared executable must be Linux/AMD64 ELF")
		}
		info, err := buildinfo.ReadFile(file)
		if err != nil {
			return err
		}
		if name == GinkgoBinary && info.Path != "github.com/onsi/ginkgo/v2/ginkgo" {
			return errors.New("prepared Ginkgo must be the framework CLI")
		}
		if name == TestBinary && !strings.HasSuffix(info.Path, ".test") {
			return errors.New("prepared suite must be a precompiled Go test binary")
		}
		version := ""
		// Go records the CLI's framework as Main, and the suite's as a dependency.
		if info.Main.Path == "github.com/onsi/ginkgo/v2" {
			if info.Main.Replace != nil {
				return errors.New("replaced Ginkgo framework not supported")
			}
			version = info.Main.Version
		}
		for _, dep := range info.Deps {
			if dep.Path == "github.com/onsi/ginkgo/v2" {
				if dep.Replace != nil {
					return errors.New("replaced Ginkgo framework not supported")
				}
				version = dep.Version
			}
		}
		linux, amd64 := false, false
		for _, setting := range info.Settings {
			if setting.Key == "GOOS" && setting.Value == "linux" {
				linux = true
			}
			if setting.Key == "GOARCH" && setting.Value == "amd64" {
				amd64 = true
			}
		}
		if version != m.GinkgoVersion || !linux || !amd64 {
			return errors.New("Ginkgo binary/framework/platform mismatch")
		}
	}
	return nil
}

func validateChart(file, root string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	compressed := bufio.NewReader(f)
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		return err
	}
	defer gz.Close()
	gz.Multistream(false)
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		total += h.Size
		if !validPath(h.Name) || !strings.HasPrefix(h.Name, root+"/") || seen[h.Name] || h.Typeflag != tar.TypeReg || h.Linkname != "" || len(h.PAXRecords) != 0 || h.Size < 0 || h.Size > 32<<20 || total > 128<<20 || len(seen) >= MaxArchiveEntries {
			return errors.New("unsafe/oversized chart archive")
		}
		seen[h.Name] = true
		// Helm dependencies are themselves archives; validate them rather than trusting an opaque nested tarball.
		if strings.HasSuffix(h.Name, ".tgz") {
			return errors.New("chart dependencies must be unpacked before packaging")
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return err
		}
	}
	if !seen[root+"/Chart.yaml"] || !seen[root+"/values.yaml"] {
		return errors.New("incomplete chart archive")
	}
	// Consume to enforce the decompression bound even for trailing data.
	extra, err := io.Copy(io.Discard, io.LimitReader(gz, 1025))
	if err != nil {
		return err
	}
	if extra != 0 {
		return errors.New("trailing chart data")
	}
	if _, err := compressed.ReadByte(); err != io.EOF {
		return errors.New("trailing compressed chart data")
	}
	return nil
}

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}
type imageIndex struct {
	SchemaVersion int          `json:"schemaVersion"`
	Manifests     []descriptor `json:"manifests"`
}
type imageManifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
}

func validateOCI(file, sourceSHA string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	blobs := map[string]FileDigest{}
	metadata := map[string][]byte{}
	seen := map[string]bool{}
	var total, metadataBytes int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		total += h.Size
		blob := strings.HasPrefix(h.Name, "blobs/sha256/") && digestPattern.MatchString("sha256:"+strings.TrimPrefix(h.Name, "blobs/sha256/"))
		if !validPath(h.Name) || (!blob && h.Name != "index.json" && h.Name != "oci-layout") || seen[h.Name] || h.Typeflag != tar.TypeReg || h.Linkname != "" || len(h.PAXRecords) != 0 || h.Size <= 0 || h.Size > MaxFileBytes || total > MaxBundleBytes || len(seen) >= MaxArchiveEntries {
			return errors.New("unsafe OCI entry")
		}
		seen[h.Name] = true
		hash := sha256.New()
		var data []byte
		if h.Size <= 1<<20 {
			metadataBytes += h.Size
			if metadataBytes > 16<<20 {
				return errors.New("OCI metadata exceeds limit")
			}
			data, err = io.ReadAll(io.TeeReader(tr, hash))
		} else {
			_, err = io.Copy(hash, tr)
		}
		if err != nil {
			return err
		}
		digest := hex.EncodeToString(hash.Sum(nil))
		if blob {
			if path.Base(h.Name) != digest {
				return errors.New("OCI blob digest mismatch")
			}
			blobs["sha256:"+digest] = FileDigest{Size: h.Size}
			if data != nil {
				metadata["sha256:"+digest] = data
			}
		} else {
			metadata[h.Name] = data
		}
	}
	if extra, err := io.Copy(io.Discard, io.LimitReader(f, 1)); err != nil || extra != 0 {
		return errors.New("trailing OCI archive data")
	}
	var layout struct {
		Version string `json:"imageLayoutVersion"`
	}
	if err := json.Unmarshal(metadata["oci-layout"], &layout); err != nil || layout.Version != "1.0.0" {
		return errors.New("invalid OCI layout")
	}
	var index imageIndex
	if err := json.Unmarshal(metadata["index.json"], &index); err != nil || index.SchemaVersion != 2 || len(index.Manifests) != 1 {
		return errors.New("OCI requires exactly one image")
	}
	referenced := map[string]bool{}
	check := func(d descriptor) error {
		entry, ok := blobs[d.Digest]
		if !ok || entry.Size != d.Size || !digestPattern.MatchString(d.Digest) {
			return errors.New("missing OCI descriptor")
		}
		referenced[d.Digest] = true
		return nil
	}
	image := index.Manifests[0]
	if err := check(image); err != nil {
		return err
	}
	var manifest imageManifest
	if err := json.Unmarshal(metadata[image.Digest], &manifest); err != nil || manifest.SchemaVersion != 2 || len(manifest.Layers) == 0 {
		return errors.New("invalid OCI image manifest")
	}
	if err := check(manifest.Config); err != nil {
		return err
	}
	for _, layer := range manifest.Layers {
		if err := check(layer); err != nil {
			return err
		}
	}
	var config struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		Config       struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if err := json.Unmarshal(metadata[manifest.Config.Digest], &config); err != nil || config.Architecture != "amd64" || config.OS != "linux" || config.Config.Labels["org.opencontainers.image.revision"] != sourceSHA {
		return errors.New("OCI platform/full source SHA mismatch")
	}
	if len(referenced) != len(blobs) {
		return errors.New("unreferenced OCI blob")
	}
	return nil
}

// VerifyDownload checks the exact server digest of the GitHub artifact ZIP, which must contain only bundle.tar.
// The receipt must have been obtained independently via Gate.Artifact, never read from that ZIP.
func VerifyDownload(file, parent string, expected Frozen, receipt Receipt) (*Bundle, error) {
	if err := receipt.Validate(expected); err != nil {
		return nil, err
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > MaxBundleBytes {
		return nil, errors.New("invalid artifact size")
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, MaxBundleBytes+1)); err != nil {
		return nil, err
	}
	if "sha256:"+hex.EncodeToString(h.Sum(nil)) != receipt.Digest {
		return nil, errors.New("artifact server digest mismatch")
	}
	zr, err := zip.NewReader(f, info.Size())
	if err != nil {
		return nil, err
	}
	if len(zr.File) != 1 {
		return nil, errors.New("unexpected artifact ZIP entries")
	}
	entry := zr.File[0]
	if entry.Name != "bundle.tar" || !entry.Mode().IsRegular() || entry.UncompressedSize64 > uint64(MaxBundleBytes) {
		return nil, errors.New("unsafe artifact ZIP")
	}
	r, err := entry.Open()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return VerifyBundle(r, parent, expected)
}

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
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// ExtractControllerLayout prepares a validated OCI directory for trusted registry
// tooling. It does not run the image, contact a registry, or choose a target.
// The caller owns removal of the returned directory; digest identifies its one image.
func (b *Bundle) ExtractControllerLayout(parent string) (_ string, _ string, retErr error) {
	if err := b.Manifest.Source.Validate(); err != nil {
		return "", "", err
	}
	archive := filepath.Join(b.Root, ControllerArchive)
	digest, err := fileDigest(archive, ControllerArchive)
	if err != nil {
		return "", "", err
	}
	matches := false
	for _, expected := range b.Manifest.Files {
		if expected == digest {
			matches = true
		}
	}
	if !matches {
		return "", "", errors.New("controller archive changed after validation")
	}
	if err := validateOCI(archive, b.Manifest.Source.HeadSHA); err != nil {
		return "", "", err
	}
	root, err := os.MkdirTemp(parent, "controller-layout-")
	if err != nil {
		return "", "", err
	}
	defer func() {
		if retErr != nil {
			os.RemoveAll(root)
		}
	}()
	f, err := os.Open(archive)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", "", err
		}
		// validateOCI has already restricted entries to regular, bounded OCI files.
		if !validPath(h.Name) || h.Typeflag != tar.TypeReg {
			return "", "", errors.New("unsafe OCI entry")
		}
		dest := filepath.Join(root, filepath.FromSlash(h.Name))
		if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			return "", "", err
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return "", "", err
		}
		_, copyErr := io.Copy(out, tr)
		if err := errors.Join(copyErr, out.Close()); err != nil {
			return "", "", err
		}
	}
	indexFile, err := os.Open(filepath.Join(root, "index.json"))
	if err != nil {
		return "", "", err
	}
	defer indexFile.Close()
	var index imageIndex
	if err := json.NewDecoder(io.LimitReader(indexFile, 1<<20)).Decode(&index); err != nil {
		return "", "", err
	}
	return root, index.Manifests[0].Digest, nil
}

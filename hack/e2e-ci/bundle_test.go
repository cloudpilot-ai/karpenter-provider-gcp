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
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

func frozenFixture(t *testing.T) Frozen {
	t.Helper()
	f, err := gateFixture(t, nil).Capture(context.Background(), validDispatch(), []string{"standard"})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestVerifyBundleRejectsTraversal(t *testing.T) {
	var data bytes.Buffer
	tw := tar.NewWriter(&data)
	if err := tw.WriteHeader(&tar.Header{Name: "../escape", Mode: 0600, Size: 1}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte("x"))
	tw.Close()
	if _, err := VerifyBundle(&data, t.TempDir(), frozenFixture(t)); err == nil {
		t.Fatal("traversal accepted")
	}
}

func marshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var fixtureBinaryOnce sync.Once
var fixtureBinaryData []byte
var fixtureBinaryErr error

func linuxBinary(t *testing.T) []byte {
	t.Helper()
	fixtureBinaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "e2eci-binary-fixture-")
		if err != nil {
			fixtureBinaryErr = err
			return
		}
		defer os.RemoveAll(dir)
		source := filepath.Join(dir, "fixture_test.go")
		if err := os.WriteFile(source, []byte("package fixture\nimport (\"testing\"; \"github.com/onsi/ginkgo/v2\")\nfunc TestCompile(t *testing.T){ginkgo.GinkgoT()}\n"), 0600); err != nil {
			fixtureBinaryErr = err
			return
		}
		command := exec.Command("go", "test", "-c", "-buildvcs=false", "-o", filepath.Join(dir, "fixture"), source)
		command.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
		if out, err := command.CombinedOutput(); err != nil {
			fixtureBinaryErr = fmt.Errorf("fixture build: %w: %s", err, out)
			return
		}
		fixtureBinaryData, fixtureBinaryErr = os.ReadFile(filepath.Join(dir, "fixture"))
	})
	if fixtureBinaryErr != nil {
		t.Fatal(fixtureBinaryErr)
	}
	return fixtureBinaryData
}

var fixtureGinkgoOnce sync.Once
var fixtureGinkgoData []byte
var fixtureGinkgoErr error

func linuxGinkgo(t *testing.T) []byte {
	t.Helper()
	fixtureGinkgoOnce.Do(func() {
		dir, err := os.MkdirTemp("", "e2eci-ginkgo-fixture-")
		if err != nil {
			fixtureGinkgoErr = err
			return
		}
		defer os.RemoveAll(dir)
		output := filepath.Join(dir, "ginkgo")
		command := exec.Command("go", "build", "-o", output, "github.com/onsi/ginkgo/v2/ginkgo")
		command.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
		if out, err := command.CombinedOutput(); err != nil {
			fixtureGinkgoErr = fmt.Errorf("Ginkgo fixture build: %w: %s", err, out)
			return
		}
		fixtureGinkgoData, fixtureGinkgoErr = os.ReadFile(output)
	})
	if fixtureGinkgoErr != nil {
		t.Fatal(fixtureGinkgoErr)
	}
	return fixtureGinkgoData
}
func tarBytes(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	tw := tar.NewWriter(&buffer)
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		data := entries[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(data)), Mode: 0600, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
func chartBytes(t *testing.T, root string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	gz.Write(tarBytes(t, map[string][]byte{root + "/Chart.yaml": []byte("apiVersion: v2\nname: " + root + "\nversion: 0.0.0\n"), root + "/values.yaml": []byte("{}\n")}))
	gz.Close()
	return buffer.Bytes()
}
func ociBytes(t *testing.T, sha string) []byte {
	t.Helper()
	files := map[string][]byte{"oci-layout": []byte(`{"imageLayoutVersion":"1.0.0"}`)}
	blob := func(data []byte, media string) descriptor {
		hash := sha256.Sum256(data)
		digest := hex.EncodeToString(hash[:])
		files["blobs/sha256/"+digest] = data
		return descriptor{Digest: "sha256:" + digest, Size: int64(len(data)), MediaType: media}
	}
	config := blob([]byte(`{"architecture":"amd64","os":"linux","config":{"Labels":{"org.opencontainers.image.revision":"`+sha+`"}}}`), "application/vnd.oci.image.config.v1+json")
	layer := blob([]byte("opaque image layer"), "application/vnd.oci.image.layer.v1.tar+gzip")
	image := blob(marshal(t, imageManifest{SchemaVersion: 2, Config: config, Layers: []descriptor{layer}}), "application/vnd.oci.image.manifest.v1+json")
	files["index.json"] = marshal(t, imageIndex{SchemaVersion: 2, Manifests: []descriptor{image}})
	return tarBytes(t, files)
}
func bundleEntries(t *testing.T, f Frozen) map[string][]byte {
	t.Helper()
	files := map[string][]byte{ControllerArchive: ociBytes(t, f.HeadSHA), ControllerChart: chartBytes(t, "karpenter"), CRDChart: chartBytes(t, "karpenter-crd"), TestBinary: linuxBinary(t), GinkgoBinary: linuxGinkgo(t)}
	m := Manifest{Schema: 1, Source: f, Platform: "linux/amd64", GinkgoVersion: "v2.33.0", Outcome: "success"}
	for name, data := range files {
		hash := sha256.Sum256(data)
		m.Files = append(m.Files, FileDigest{Name: name, Size: int64(len(data)), SHA256: hex.EncodeToString(hash[:])})
	}
	files[ManifestName] = marshal(t, m)
	return files
}
func refreshManifest(t *testing.T, entries map[string][]byte) {
	var m Manifest
	if err := json.Unmarshal(entries[ManifestName], &m); err != nil {
		t.Fatal(err)
	}
	for i := range m.Files {
		entry := &m.Files[i]
		data := entries[entry.Name]
		sum := sha256.Sum256(data)
		entry.Size = int64(len(data))
		entry.SHA256 = hex.EncodeToString(sum[:])
	}
	entries[ManifestName] = marshal(t, m)
}

func TestBundleRoundTripAndExactArtifactDigest(t *testing.T) {
	f := frozenFixture(t)
	entries := bundleEntries(t, f)
	raw := tarBytes(t, entries)
	bundle, err := VerifyBundle(bytes.NewReader(raw), t.TempDir(), f)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(bundle.Root)
	layout, imageDigest, err := bundle.ExtractControllerLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(layout)
	if !digestPattern.MatchString(imageDigest) {
		t.Fatal("missing exact image digest")
	}
	if _, err := os.Stat(filepath.Join(layout, "index.json")); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{TestBinary, GinkgoBinary} {
		info, err := os.Stat(filepath.Join(bundle.Root, name))
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatalf("binary not made executable after validation: %v", err)
		}
	}
	output := filepath.Join(t.TempDir(), "bundle.tar")
	if err := WriteBundle(bundle.Root, output, f, "v2.33.0"); err != nil {
		t.Fatal(err)
	}
	packed, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var zipData bytes.Buffer
	zw := zip.NewWriter(&zipData)
	entry, err := zw.Create("bundle.tar")
	if err != nil {
		t.Fatal(err)
	}
	entry.Write(packed)
	zw.Close()
	sum := sha256.Sum256(zipData.Bytes())
	receipt := Receipt{Schema: 1, ID: 123, Name: ArtifactName(f), Digest: "sha256:" + hex.EncodeToString(sum[:]), RunID: 42, RunAttempt: 1, WorkflowSHA: toolSHA}
	file := filepath.Join(t.TempDir(), "artifact.zip")
	os.WriteFile(file, zipData.Bytes(), 0600)
	verified, err := VerifyDownload(file, t.TempDir(), f, receipt)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(verified.Root)
	receipt.Digest = "sha256:" + strings.Repeat("0", 64)
	if _, err := VerifyDownload(file, t.TempDir(), f, receipt); err == nil {
		t.Fatal("wrong server digest accepted")
	}
	receipt.RunAttempt = 2
	if _, err := VerifyDownload(file, t.TempDir(), f, receipt); err == nil {
		t.Fatal("wrong artifact attempt accepted")
	}
}

func TestVerifyBundleRejectsUntrustedContents(t *testing.T) {
	f := frozenFixture(t)
	good := bundleEntries(t, f)
	for _, tc := range []struct {
		name   string
		change func(map[string][]byte)
	}{
		{"unexpected executable", func(e map[string][]byte) { e["coordinator"] = []byte("not trusted") }},
		{"absolute", func(e map[string][]byte) { e["/etc/passwd"] = []byte("x") }},
		{"missing", func(e map[string][]byte) { delete(e, TestBinary) }},
		{"digest", func(e map[string][]byte) { e[TestBinary] = []byte("tampered") }},
		{"source", func(e map[string][]byte) {
			var m Manifest
			json.Unmarshal(e[ManifestName], &m)
			m.Source.HeadSHA = toolSHA
			e[ManifestName] = marshal(t, m)
		}},
		{"attempt", func(e map[string][]byte) {
			var m Manifest
			json.Unmarshal(e[ManifestName], &m)
			m.Source.Dispatch.RunAttempt = 2
			e[ManifestName] = marshal(t, m)
		}},
		{"platform", func(e map[string][]byte) {
			var m Manifest
			json.Unmarshal(e[ManifestName], &m)
			m.Platform = "linux/arm64"
			e[ManifestName] = marshal(t, m)
		}},
		{"ginkgo version", func(e map[string][]byte) {
			var m Manifest
			json.Unmarshal(e[ManifestName], &m)
			m.GinkgoVersion = "v2.32.0"
			e[ManifestName] = marshal(t, m)
		}},
		{"wrong image source", func(e map[string][]byte) { e[ControllerArchive] = ociBytes(t, toolSHA); refreshManifest(t, e) }},
		{"unsafe chart", func(e map[string][]byte) {
			var b bytes.Buffer
			gz := gzip.NewWriter(&b)
			gz.Write(tarBytes(t, map[string][]byte{"../escape": []byte("x")}))
			gz.Close()
			e[ControllerChart] = b.Bytes()
			refreshManifest(t, e)
		}},
		{"unknown manifest field", func(e map[string][]byte) {
			e[ManifestName] = append(e[ManifestName][:len(e[ManifestName])-1], []byte(`,"target":"evil"}`)...)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries := map[string][]byte{}
			for name, data := range good {
				entries[name] = bytes.Clone(data)
			}
			tc.change(entries)
			parent := t.TempDir()
			if _, err := VerifyBundle(bytes.NewReader(tarBytes(t, entries)), parent, f); err == nil {
				t.Fatal("bad bundle accepted")
			}
			files, err := os.ReadDir(parent)
			if err != nil || len(files) != 0 {
				t.Fatal("rejected bundle left extracted data")
			}
		})
	}
}

func TestVerifyBundleRejectsLinksDuplicatesAndOversize(t *testing.T) {
	f := frozenFixture(t)
	for _, header := range []*tar.Header{
		{Name: TestBinary, Typeflag: tar.TypeSymlink, Linkname: "/bin/sh"},
		{Name: TestBinary, Typeflag: tar.TypeLink, Linkname: "ginkgo"},
		{Name: TestBinary, Typeflag: tar.TypeReg, Size: MaxFileBytes + 1},
		{Name: TestBinary, Typeflag: tar.TypeDir},
	} {
		var data bytes.Buffer
		tw := tar.NewWriter(&data)
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyBundle(&data, t.TempDir(), f); err == nil {
			t.Fatalf("unsafe header accepted: %+v", header)
		}
	}
	var data bytes.Buffer
	tw := tar.NewWriter(&data)
	for i := 0; i < 2; i++ {
		tw.WriteHeader(&tar.Header{Name: TestBinary, Typeflag: tar.TypeReg, Size: 1})
		tw.Write([]byte("x"))
	}
	tw.Close()
	if _, err := VerifyBundle(&data, t.TempDir(), f); err == nil {
		t.Fatal("duplicate entry accepted")
	}
}

func TestVerifyBundleRejectsTrailingNestedArchives(t *testing.T) {
	f := frozenFixture(t)
	good := bundleEntries(t, f)
	for _, name := range []string{ControllerArchive, ControllerChart} {
		t.Run(name, func(t *testing.T) {
			entries := map[string][]byte{}
			for key, data := range good {
				entries[key] = bytes.Clone(data)
			}
			entries[name] = append(entries[name], []byte("hidden archive")...)
			refreshManifest(t, entries)
			if _, err := VerifyBundle(bytes.NewReader(tarBytes(t, entries)), t.TempDir(), f); err == nil {
				t.Fatal("trailing archive bytes accepted")
			}
		})
	}
}

func TestVerifyBundleRejectsWrongExecutableRole(t *testing.T) {
	f := frozenFixture(t)
	entries := bundleEntries(t, f)
	entries[GinkgoBinary] = linuxBinary(t)
	refreshManifest(t, entries)
	if _, err := VerifyBundle(bytes.NewReader(tarBytes(t, entries)), t.TempDir(), f); err == nil {
		t.Fatal("suite executable accepted as Ginkgo CLI")
	}
}

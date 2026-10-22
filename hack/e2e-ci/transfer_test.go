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
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicReportRejectsPathTraversal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	entry, err := w.Create("../outcome.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = entry.Write([]byte(`{}`))
	_ = w.Close()
	_ = f.Close()
	if err := ExtractPublicReport(path, filepath.Join(t.TempDir(), "out")); err == nil {
		t.Fatal("report traversal accepted")
	}
}

func TestPublicReportRejectsUnsafeEntries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []string
		symlink bool
	}{
		{"absolute", []string{"/outcome.json"}, false}, {"backslash", []string{`..\outcome.json`}, false}, {"unknown", []string{"outcome.json", "credentials.json"}, false}, {"duplicate", []string{"outcome.json", "outcome.json"}, false}, {"symlink", []string{"outcome.json"}, true}, {"missing outcome", []string{"results.json"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "report.zip")
			f, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			w := zip.NewWriter(f)
			for _, name := range tc.entries {
				header := &zip.FileHeader{Name: name}
				if tc.symlink {
					header.SetMode(os.ModeSymlink | 0600)
				} else {
					header.SetMode(0600)
				}
				entry, err := w.CreateHeader(header)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = entry.Write([]byte(`{}`))
			}
			_ = w.Close()
			_ = f.Close()
			dir := filepath.Join(t.TempDir(), "out")
			if err := ExtractPublicReport(path, dir); err == nil {
				t.Fatal("unsafe artifact accepted")
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("partial extraction survived")
			}
		})
	}
}

func TestOriginalZIPDownloadDropsAuthorizationAtStorage(t *testing.T) {
	payload := []byte("original ZIP bytes, not extracted/repacked")
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/zip") {
			if r.Header.Get("Authorization") != "Bearer synthetic-test" {
				t.Error("missing GitHub auth")
			}
			w.Header().Set("Location", server.URL+"/storage")
			w.WriteHeader(302)
			return
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("forwarded GitHub authorization to artifact storage")
		}
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "artifact.zip")
	g := Gate{Client: server.Client(), APIURL: server.URL, Token: "synthetic-test"}
	if err := g.Download(context.Background(), 17, file); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("ZIP changed %v", err)
	}
	if err := g.Download(context.Background(), 17, file); err == nil {
		t.Fatal("existing file overwritten")
	}
	sum := sha256.Sum256(payload)
	if err := VerifyReportDigest(file, "sha256:"+hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	if err := VerifyReportDigest(file, "sha256:"+strings.Repeat("0", 64)); err == nil {
		t.Fatal("wrong digest accepted")
	}
}

func TestArtifactTransportErrorsDoNotExposeSignedURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://127.0.0.1:1/storage?sig=synthetic-private-signature")
		w.WriteHeader(302)
	}))
	defer server.Close()
	err := (Gate{Client: server.Client(), APIURL: server.URL}).Download(context.Background(), 1, filepath.Join(t.TempDir(), "download.zip"))
	if err == nil || strings.Contains(err.Error(), "synthetic-private-signature") {
		t.Fatalf("unsafe transport error: %v", err)
	}
}

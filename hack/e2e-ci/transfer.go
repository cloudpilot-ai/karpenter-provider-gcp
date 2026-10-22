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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Download preserves the original server ZIP. Authorization is used only on the
// GitHub request, never on the signed artifact-storage redirect.
func (g Gate) Download(ctx context.Context, id int64, destination string) error {
	if id <= 0 {
		return errors.New("invalid artifact ID")
	}
	base := g.APIURL
	if base == "" {
		base = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/actions/artifacts/%d/zip", base, Repository, id), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	client := g.Client
	if client == nil {
		client = http.DefaultClient
	}
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := c.Do(req)
	if err != nil {
		return err
	}
	if res.StatusCode == http.StatusFound {
		location := res.Header.Get("Location")
		_ = res.Body.Close()
		u, err := url.Parse(location)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return errors.New("invalid artifact redirect")
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
		if err != nil {
			return err
		}
		res, err = c.Do(req)
		if err != nil {
			// net/url errors include signed storage URLs; never emit those credentials.
			return errors.New("artifact storage download failed")
		}
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("artifact download returned %d", res.StatusCode)
	}
	f, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(f, io.LimitReader(res.Body, MaxBundleBytes+1))
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil || n > MaxBundleBytes {
		_ = os.Remove(destination)
		return errors.Join(copyErr, closeErr, errors.New("artifact transfer incomplete or oversized"))
	}
	return nil
}

func ExtractPublicReport(path, destination string) (retErr error) {
	z, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer z.Close()
	allowed := map[string]bool{"outcome.json": true, "results.json": true, "junit.xml": true, "suite.log": true, "controller.log": true}
	if len(z.File) < 1 || len(z.File) > len(allowed) {
		return errors.New("invalid public report entry count")
	}
	if err := os.Mkdir(destination, 0700); err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			_ = os.RemoveAll(destination)
		}
	}()
	seen := map[string]bool{}
	var total uint64
	for _, entry := range z.File {
		total += entry.UncompressedSize64
		if !allowed[entry.Name] || seen[entry.Name] || !entry.Mode().IsRegular() || entry.UncompressedSize64 > 16<<20 || total > 40<<20 {
			return errors.New("unsafe or oversized public report entry")
		}
		seen[entry.Name] = true
		r, err := entry.Open()
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(r, (16<<20)+1))
		closeErr := r.Close()
		if err != nil || closeErr != nil {
			return errors.Join(err, closeErr)
		}
		if len(data) > 16<<20 {
			return errors.New("oversized public report")
		}
		// #nosec G305 -- entry.Name passed the fixed five-basename allowlist above.
		if err := os.WriteFile(filepath.Join(destination, entry.Name), data, 0600); err != nil {
			return err
		}
	}
	if !seen["outcome.json"] {
		return errors.New("missing outcome")
	}
	return nil
}
func VerifyReportDigest(path, digest string) error {
	if !digestPattern.MatchString(digest) {
		return errors.New("invalid report digest")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, (40<<20)+1))
	if err != nil {
		return err
	}
	if n > 40<<20 || hex.EncodeToString(h.Sum(nil)) != strings.TrimPrefix(digest, "sha256:") {
		return errors.New("report ZIP digest mismatch")
	}
	return nil
}

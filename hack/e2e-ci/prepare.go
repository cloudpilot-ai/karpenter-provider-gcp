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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"
)

// Preparation runs only on the disposable credential-free build worker. The
// worker is considered compromised after PR compilation/dependency processing.
type preparation struct{ env []string }
type cappedOutput struct{ bytes.Buffer }

func (b *cappedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 1<<20 {
		return 0, errors.New("build command output exceeds limit")
	}
	return b.Buffer.Write(p)
}
func (p preparation) run(ctx context.Context, cwd, command string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = cwd
	cmd.Env = append([]string(nil), p.env...)
	// ko rejects GOOS/GOARCH together with --platform. The complete allowlisted
	// environment excludes inherited values; only Go compilation receives them.
	if command == "go" {
		cmd.Env = append(cmd.Env, "GOOS=linux", "GOARCH=amd64")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	var out, stderr cappedOutput
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s failed: %w", command, err)
	}
	return out.Bytes(), nil
}
func newPreparation(work string) (preparation, error) {
	for _, name := range []string{"home", "tmp", "gopath", "gocache", "modcache", "helm", "docker"} {
		if err := os.Mkdir(filepath.Join(work, name), 0700); err != nil {
			return preparation{}, err
		}
	}
	config := filepath.Join(work, "ko.yaml")
	if err := os.WriteFile(config, []byte("defaultPlatforms:\n  - linux/amd64\n"), 0600); err != nil {
		return preparation{}, err
	}
	return preparation{env: []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Join(work, "home"), "TMPDIR=" + filepath.Join(work, "tmp"),
		"GOPATH=" + filepath.Join(work, "gopath"), "GOCACHE=" + filepath.Join(work, "gocache"), "GOMODCACHE=" + filepath.Join(work, "modcache"),
		"GOENV=off", "GOWORK=off", "GOFLAGS=-buildvcs=true", "CGO_ENABLED=0", "GOPROXY=https://proxy.golang.org,direct", "GOSUMDB=sum.golang.org",
		"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_COUNT=0",
		"KO_CONFIG_PATH=" + config, "KO_DOCKER_REPO=e2e.invalid/karpenter", "DOCKER_CONFIG=" + filepath.Join(work, "docker"),
		"HELM_CONFIG_HOME=" + filepath.Join(work, "helm"), "HELM_CACHE_HOME=" + filepath.Join(work, "helm"), "HELM_DATA_HOME=" + filepath.Join(work, "helm"),
	}}, nil
}

// FetchSource uses only the upstream PR ref and fails closed if it has moved or
// vanished. It never fetches a contributor branch or runs checkout credentials/hooks.
func FetchSource(ctx context.Context, destination string, f Frozen) (retErr error) {
	if err := f.Validate(); err != nil {
		return err
	}
	destination, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	if err := os.Mkdir(destination, 0700); err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			os.RemoveAll(destination)
		}
	}()
	work, err := os.MkdirTemp("", "e2e-fetch-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	p, err := newPreparation(work)
	if err != nil {
		return err
	}
	for _, args := range [][]string{{"-c", "core.hooksPath=/dev/null", "init", "--quiet"}, {"-c", "core.hooksPath=/dev/null", "-c", "protocol.file.allow=never", "fetch", "--no-tags", "--depth=1", "https://github.com/" + Repository + ".git", fmt.Sprintf("refs/pull/%d/head", f.PRNumber)}} {
		if _, err := p.run(ctx, destination, "git", args...); err != nil {
			return err
		}
	}
	sha, err := p.run(ctx, destination, "git", "rev-parse", "FETCH_HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(sha)) != f.HeadSHA {
		return errors.New("upstream PR ref no longer resolves to frozen SHA; dispatch again")
	}
	_, err = p.run(ctx, destination, "git", "-c", "core.hooksPath=/dev/null", "checkout", "--detach", f.HeadSHA)
	return err
}

// Prepare compiles with trusted argv and an explicit PR cwd, never a PR Makefile,
// shell script or local Action. Its caller MUST provide a worker with no credentials.
func Prepare(ctx context.Context, source, output string, f Frozen) error {
	if err := f.Validate(); err != nil {
		return err
	}
	source, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(output))
	if err != nil {
		return err
	}
	output = filepath.Join(parent, filepath.Base(output))
	rel, err := filepath.Rel(source, output)
	if err != nil {
		return err
	}
	if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("bundle output must be outside PR checkout")
	}
	work, err := os.MkdirTemp("", "e2e-prepare-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	p, err := newPreparation(work)
	if err != nil {
		return err
	}
	sha, err := p.run(ctx, source, "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(sha)) != f.HeadSHA {
		return errors.New("checkout does not match captured full SHA")
	}
	status, err := p.run(ctx, source, "git", "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return err
	}
	if len(status) != 0 {
		return errors.New("PR checkout must be clean before preparation")
	}
	// Reject linked chart/source inputs before trusted tools follow them. Compilation
	// remains arbitrary PR code, but must not read through checkout-owned symlinks.
	if err := filepath.WalkDir(source, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git") {
			return filepath.SkipDir
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("linked PR input %s", path)
		}
		return nil
	}); err != nil {
		return err
	}
	moduleData, err := p.run(ctx, source, "go", "list", "-m", "-json", "github.com/onsi/ginkgo/v2")
	if err != nil {
		return err
	}
	var framework struct {
		Path, Version string
		Replace       any
	}
	if err := json.Unmarshal(moduleData, &framework); err != nil {
		return err
	}
	if framework.Path != "github.com/onsi/ginkgo/v2" || !versionPattern.MatchString(framework.Version) || framework.Replace != nil {
		return errors.New("unsupported/replaced Ginkgo module")
	}
	files := filepath.Join(work, "files")
	if err := os.Mkdir(files, 0700); err != nil {
		return err
	}
	layout := filepath.Join(work, "oci")
	if _, err := p.run(ctx, source, "ko", "build", "--push=false", "--sbom=none", "--platform=linux/amd64", "--oci-layout-path="+layout, "--image-label=org.opencontainers.image.revision="+f.HeadSHA, "--ldflags=-X=sigs.k8s.io/karpenter/pkg/operator.Version="+f.HeadSHA, "./cmd/controller"); err != nil {
		return err
	}
	if err := archiveDirectory(layout, filepath.Join(files, ControllerArchive)); err != nil {
		return err
	}
	for _, chart := range []struct{ name, dest string }{{"karpenter", ControllerChart}, {"karpenter-crd", CRDChart}} {
		chartDir := filepath.Join(source, "charts", chart.name)
		metadata, err := os.ReadFile(filepath.Join(chartDir, "Chart.yaml"))
		if err != nil {
			return err
		}
		var chartMetadata struct {
			Dependencies []yaml.Node `yaml:"dependencies"`
		}
		if err := yaml.Unmarshal(metadata, &chartMetadata); err != nil {
			return err
		}
		lockBefore, err := os.ReadFile(filepath.Join(chartDir, "Chart.lock"))
		hasLock := err == nil
		if !hasLock && (!os.IsNotExist(err) || len(chartMetadata.Dependencies) != 0) {
			return errors.New("chart requires locked dependencies")
		}
		if _, err := p.run(ctx, source, "helm", "dependency", "build", chartDir, "--skip-refresh"); err != nil {
			return err
		}
		if hasLock {
			lockAfter, err := os.ReadFile(filepath.Join(chartDir, "Chart.lock"))
			if err != nil || !bytes.Equal(lockBefore, lockAfter) {
				return errors.New("Helm dependency lock changed")
			}
		}
		packaged := filepath.Join(work, "package-"+chart.name)
		if err := os.Mkdir(packaged, 0700); err != nil {
			return err
		}
		if _, err := p.run(ctx, source, "helm", "package", chartDir, "--destination", packaged); err != nil {
			return err
		}
		entries, err := os.ReadDir(packaged)
		if err != nil {
			return err
		}
		if len(entries) != 1 || entries[0].IsDir() || !strings.HasSuffix(entries[0].Name(), ".tgz") {
			return errors.New("unexpected Helm package output")
		}
		if err := os.Rename(filepath.Join(packaged, entries[0].Name()), filepath.Join(files, chart.dest)); err != nil {
			return err
		}
	}
	if _, err := p.run(ctx, source, "go", "test", "-c", "-buildvcs=true", "-o", filepath.Join(files, TestBinary), "./test/suites"); err != nil {
		return err
	}
	if _, err := p.run(ctx, source, "go", "build", "-buildvcs=true", "-o", filepath.Join(files, GinkgoBinary), "github.com/onsi/ginkgo/v2/ginkgo"); err != nil {
		return err
	}
	// Dependency commands must not silently change tracked source or add test inputs.
	sha, err = p.run(ctx, source, "git", "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(string(sha)) != f.HeadSHA {
		return errors.New("source changed during build")
	}
	status, err = p.run(ctx, source, "git", "status", "--porcelain", "--untracked-files=all")
	if err != nil || len(status) != 0 {
		return errors.New("source changed during build")
	}
	return WriteBundle(files, output, f, framework.Version)
}

func archiveDirectory(root, output string) (retErr error) {
	f, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	tw := tar.NewWriter(f)
	defer func() { retErr = errors.Join(retErr, tw.Close()) }()
	return filepath.WalkDir(root, func(file string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("non-regular build output")
		}
		name, err := filepath.Rel(root, file)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(name), Mode: 0600, Size: info.Size(), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		in, err := os.Open(file)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, in)
		return errors.Join(copyErr, in.Close())
	})
}

// WriteBundle is also useful for offline compatibility fixtures. It publishes
// atomically, refuses unknown files/links and validates contents before packaging.
func WriteBundle(root, output string, f Frozen, version string) (retErr error) {
	if err := f.Validate(); err != nil {
		return err
	}
	if !versionPattern.MatchString(version) {
		return errors.New("invalid Ginkgo version")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	if len(entries) != len(bundleFiles) {
		return errors.New("unexpected bundle contents")
	}
	m := Manifest{Schema: 1, Source: f, Platform: "linux/amd64", GinkgoVersion: version, Outcome: "success"}
	allowed := map[string]bool{}
	for _, name := range bundleFiles {
		allowed[name] = true
	}
	for _, entry := range entries {
		if !allowed[entry.Name()] || !entry.Type().IsRegular() {
			return errors.New("unsafe/unknown bundle file")
		}
		digest, err := fileDigest(filepath.Join(root, entry.Name()), entry.Name())
		if err != nil {
			return err
		}
		m.Files = append(m.Files, digest)
	}
	if err := validateContents(root, m); err != nil {
		return err
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Name < m.Files[j].Name })
	manifest, err := json.Marshal(m)
	if err != nil {
		return err
	}
	out, err := os.CreateTemp(filepath.Dir(output), ".bundle-")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	tw := tar.NewWriter(out)
	err = tw.WriteHeader(&tar.Header{Name: ManifestName, Mode: 0600, Size: int64(len(manifest)), Typeflag: tar.TypeReg})
	if err == nil {
		_, err = tw.Write(manifest)
	}
	for _, file := range m.Files {
		if err != nil {
			break
		}
		err = tw.WriteHeader(&tar.Header{Name: file.Name, Mode: 0600, Size: file.Size, Typeflag: tar.TypeReg})
		if err != nil {
			break
		}
		in, openErr := os.Open(filepath.Join(root, file.Name))
		if openErr != nil {
			err = openErr
			break
		}
		_, copyErr := io.Copy(tw, in)
		err = errors.Join(copyErr, in.Close())
	}
	err = errors.Join(err, tw.Close(), out.Close())
	if err != nil {
		return err
	}
	// Link rather than rename so an existing output is never silently replaced.
	return os.Link(out.Name(), output)
}

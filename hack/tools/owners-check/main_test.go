package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadRootApprovers(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "OWNERS")
	if err := os.WriteFile(path, []byte(`
# comment
approvers:
- alice
- "bob"
reviewers:
- carol
emeritus_approvers:
- dave
`), 0o600); err != nil {
		t.Fatalf("write OWNERS: %v", err)
	}

	approvers, err := readRootApprovers(path)
	if err != nil {
		t.Fatalf("readRootApprovers() returned error: %v", err)
	}
	if !approvers["alice"] || !approvers["bob"] {
		t.Fatalf("approvers = %#v, want alice and bob", approvers)
	}
	if approvers["carol"] || approvers["dave"] {
		t.Fatalf("approvers = %#v, want reviewers and emeritus approvers ignored", approvers)
	}
}

func TestReadRootApproversRequiresApprovers(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "OWNERS")
	if err := os.WriteFile(path, []byte("reviewers:\n- alice\n"), 0o600); err != nil {
		t.Fatalf("write OWNERS: %v", err)
	}
	if _, err := readRootApprovers(path); err == nil {
		t.Fatal("readRootApprovers() succeeded, want error")
	}
}

func TestSetOutputWritesGitHubOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outputs")
	t.Setenv("GITHUB_OUTPUT", path)
	if err := setOutput("allowed", "true"); err != nil {
		t.Fatalf("setOutput() returned error: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if got := string(data); got != "allowed=true\n" {
		t.Fatalf("output = %q, want allowed=true", got)
	}
}

func TestSetOutputPrintsWithoutGitHubOutput(t *testing.T) {
	t.Setenv("GITHUB_OUTPUT", "")
	var b strings.Builder
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	if err := setOutput("allowed", "false"); err != nil {
		t.Fatalf("setOutput() returned error: %v", err)
	}
	_ = w.Close()
	os.Stdout = old
	if _, err := io.Copy(&b, r); err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	if got := b.String(); got != "allowed=false\n" {
		t.Fatalf("stdout = %q, want allowed=false", got)
	}
}

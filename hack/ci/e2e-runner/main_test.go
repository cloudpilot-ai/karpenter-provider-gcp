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

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestResolvePreset(t *testing.T) {
	suites := t.TempDir()
	for _, name := range []string{"provisioning", "gpu", "drift"} {
		makeSuite(t, suites, name)
	}

	for preset, want := range map[string][]string{
		"standard":     {"drift", "provisioning"},
		"gpu":          {"gpu"},
		"provisioning": {"provisioning"},
		"full":         {"drift", "gpu", "provisioning"},
	} {
		got, err := resolvePreset(suites, preset, "", "")
		if err != nil {
			t.Fatalf("resolve %s: %v", preset, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("resolve %s = %v, want %v", preset, got, want)
		}
	}
}

func TestResolvePresetRejectsInvalidSelection(t *testing.T) {
	suites := t.TempDir()
	makeSuite(t, suites, "provisioning")

	for _, tc := range []struct {
		name, preset, suite, focus string
	}{
		{name: "unknown preset", preset: "unknown"},
		{name: "missing gpu", preset: "gpu"},
		{name: "unknown focused suite", preset: "standard", suite: "missing"},
		{name: "focus without suite", preset: "standard", focus: "spec name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := resolvePreset(suites, tc.preset, tc.suite, tc.focus); err == nil {
				t.Fatal("expected selection error")
			}
		})
	}
}

func makeSuite(t *testing.T, suites, name string) {
	t.Helper()
	path := filepath.Join(suites, name)
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "suite_test.go"), []byte("package suite\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

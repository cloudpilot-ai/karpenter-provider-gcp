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
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	if err := runCLI(os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func resolvePreset(suitesDir, preset, suite, focus string) ([]string, error) {
	if focus != "" && suite == "" {
		return nil, fmt.Errorf("FOCUS requires SUITE")
	}
	if suite != "" {
		if preset != "standard" && preset != "provisioning" && preset != "gpu" && preset != "full" {
			return nil, fmt.Errorf("unknown preset %q", preset)
		}
		if !hasTests(suitesDir, suite) {
			return nil, fmt.Errorf("unknown or zero-test suite %q", suite)
		}
		return []string{suite}, nil
	}

	entries, err := os.ReadDir(suitesDir)
	if err != nil {
		return nil, fmt.Errorf("read suite inventory: %w", err)
	}
	var suites []string
	for _, entry := range entries {
		if entry.IsDir() && hasTests(suitesDir, entry.Name()) {
			suites = append(suites, entry.Name())
		}
	}
	sort.Strings(suites)

	var selected []string
	switch preset {
	case "standard":
		for _, name := range suites {
			if name != "gpu" {
				selected = append(selected, name)
			}
		}
	case "provisioning":
		for _, name := range suites {
			if name == "provisioning" {
				selected = append(selected, name)
			}
		}
	case "gpu":
		for _, name := range suites {
			if name == "gpu" {
				selected = append(selected, name)
			}
		}
	case "full":
		selected = suites
	default:
		return nil, fmt.Errorf("unknown preset %q", preset)
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("preset %q selects no suites", preset)
	}
	return selected, nil
}

func hasTests(suitesDir, suite string) bool {
	entries, err := os.ReadDir(filepath.Join(suitesDir, suite))
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), "_test.go") {
			return true
		}
	}
	return false
}

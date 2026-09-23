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
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

// Presets filter discovered suites, so new suites join standard and all automatically.
var presets = map[string]func(string) bool{
	"standard":     func(name string) bool { return name != "gpu" },
	"gpu":          func(name string) bool { return name == "gpu" },
	"all":          func(string) bool { return true },
	"provisioning": func(name string) bool { return name == "provisioning" },
}

func selectSuites(dir, name string) ([]string, error) {
	selected, ok := presets[name]
	if !ok {
		return nil, fmt.Errorf("unknown e2e preset %q (choose standard, gpu, all, or provisioning)", name)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var suites []string
	for _, entry := range entries {
		if entry.IsDir() && selected(entry.Name()) {
			suites = append(suites, "./test/suites/"+entry.Name()+"/")
		}
	}
	if len(suites) == 0 {
		return nil, fmt.Errorf("no e2e suites found for preset %q in %s", name, dir)
	}
	return suites, nil
}

func main() {
	var name string
	cmd := &cobra.Command{
		Use:          "e2e-runner",
		Short:        "Run e2e suites by preset",
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, ginkgoArgs []string) error {
			suites, err := selectSuites("test/suites", name)
			if err != nil {
				return err
			}
			args := append([]string{"run", "github.com/onsi/ginkgo/v2/ginkgo"}, ginkgoArgs...)
			args = append(args, suites...)
			runner := exec.CommandContext(cmd.Context(), "go", args...)
			runner.Stdin, runner.Stdout, runner.Stderr = os.Stdin, os.Stdout, os.Stderr
			return runner.Run()
		},
	}
	cmd.Flags().StringVar(&name, "preset", "standard", "Suite preset: standard, gpu, all, provisioning")
	if err := cmd.Execute(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.ExitCode())
		}
		os.Exit(1)
	}
}

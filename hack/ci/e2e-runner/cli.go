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
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const schemaVersion = 1

type Target struct {
	Project  string `json:"project"`
	Region   string `json:"region"`
	Location string `json:"location"`
	Prefix   string `json:"prefix"`
}

type RunPlan struct {
	Version   int              `json:"version"`
	CreatedAt time.Time        `json:"createdAt"`
	Target    Target           `json:"target"`
	Preset    string           `json:"preset"`
	Suite     string           `json:"suite,omitempty"`
	Focus     string           `json:"focus,omitempty"`
	Suites    []string         `json:"suites"`
	Quota     QuotaSnapshot    `json:"quota"`
	Waves     [][]PlannedSuite `json:"waves"`
}

func runCLI(args []string, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("expected command: plan, run, or report")
	}
	switch args[0] {
	case "run":
		return runCommand(args[1:], stderr)
	case "report":
		return reportCommand(args[1:], stderr)
	case "plan":
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	flags.SetOutput(stderr)
	suitesDir := flags.String("suites-dir", "test/suites", "suite directory")
	preset := flags.String("preset", "", "standard, gpu, or full")
	suite := flags.String("suite", "", "one suite")
	focus := flags.String("focus", "", "Ginkgo focus")
	project := flags.String("project", "", "GCP project")
	region := flags.String("region", "", "GCP region")
	location := flags.String("location", "", "GCP location")
	prefix := flags.String("prefix", "", "target prefix")
	output := flags.String("output", "", "plan JSON path")
	discover := flags.Bool("discover-quota", false, "discover quota and usage with gcloud")
	regionalCPUs := flags.Int("regional-cpus", 0, "regional CPU quota (test override)")
	globalCPUs := flags.Int("global-cpus", 0, "global CPU quota (test override)")
	addresses := flags.Int("addresses", 0, "address quota (test override)")
	diskGiB := flags.Int("disk-gib", 0, "disk quota GiB (test override)")
	gpus := flags.Int("gpus", 0, "GPU quota (test override)")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	if *preset == "" || *project == "" || *region == "" || *location == "" || *prefix == "" || *output == "" {
		return fmt.Errorf("preset, project, region, location, prefix, and output are required")
	}

	suites, err := resolvePreset(*suitesDir, *preset, *suite, *focus)
	if err != nil {
		return err
	}
	quota := QuotaSnapshot{CapturedAt: time.Now().UTC(), RegionalCPUs: *regionalCPUs, GlobalCPUs: *globalCPUs, Addresses: *addresses, DiskGiB: *diskGiB, GPUs: *gpus}
	if *discover {
		if *regionalCPUs != 0 || *globalCPUs != 0 || *addresses != 0 || *diskGiB != 0 || *gpus != 0 {
			return fmt.Errorf("discover-quota cannot be combined with quota overrides")
		}
		quota, err = discoverQuota(*project, *region)
		if err != nil {
			return err
		}
	}
	waves, err := planWaves(suites, defaultProfiles(), quota)
	if err != nil {
		return err
	}
	plan := RunPlan{Version: schemaVersion, CreatedAt: time.Now().UTC(), Target: Target{Project: *project, Region: *region, Location: *location, Prefix: *prefix}, Preset: *preset, Suite: *suite, Focus: *focus, Suites: suites, Quota: quota, Waves: waves}
	data, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*output), 0o750); err != nil {
		return err
	}
	return os.WriteFile(*output, append(data, '\n'), 0o600)
}

func defaultProfiles() map[string]ResourceProfile {
	return map[string]ResourceProfile{
		"channel-image-selection": {CPUs: 2, Addresses: 1, DiskGiB: 50},
		"confidential":            {CPUs: 2, Addresses: 1, DiskGiB: 50},
		"consolidation":           {CPUs: 4, Addresses: 2, DiskGiB: 100},
		"drift":                   {CPUs: 4, Addresses: 2, DiskGiB: 100},
		"expiration":              {CPUs: 4, Addresses: 2, DiskGiB: 100},
		"gc":                      {CPUs: 2, Addresses: 1, DiskGiB: 50},
		"gpu":                     {CPUs: 4, Addresses: 1, DiskGiB: 50, GPUs: 1},
		"kubelet_config":          {CPUs: 2, Addresses: 1, DiskGiB: 50},
		"networking":              {CPUs: 2, Addresses: 1, DiskGiB: 50},
		"provisioning":            {CPUs: 2, Addresses: 1, DiskGiB: 50, Workers: 1},
		"repair":                  {CPUs: 4, Addresses: 2, DiskGiB: 100},
		"storage":                 {CPUs: 2, Addresses: 1, DiskGiB: 50},
	}
}

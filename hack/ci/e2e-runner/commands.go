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
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func runCommand(args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	planPath := flags.String("plan", "", "plan JSON path")
	artifacts := flags.String("artifacts", "", "artifact directory")
	output := flags.String("output", "", "result JSON path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *planPath == "" || *artifacts == "" || *output == "" {
		return fmt.Errorf("plan, artifacts, and output are required")
	}
	data, err := os.ReadFile(*planPath)
	if err != nil {
		return err
	}
	var plan RunPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return err
	}
	result, runErr := executePlan(context.Background(), plan, *artifacts)
	if err := writeResult(*output, result); err != nil {
		return err
	}
	return runErr
}

func reportCommand(args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("report", flag.ContinueOnError)
	flags.SetOutput(stderr)
	input := flags.String("input", "", "result JSON path")
	output := flags.String("output", "", "Markdown output path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *input == "" || *output == "" {
		return fmt.Errorf("input and output are required")
	}
	data, err := os.ReadFile(*input)
	if err != nil {
		return err
	}
	var result RunResult
	if err := json.Unmarshal(data, &result); err != nil {
		return err
	}
	report, err := renderReport(result)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*output), 0o750); err != nil {
		return err
	}
	return os.WriteFile(*output, []byte(report), 0o600)
}

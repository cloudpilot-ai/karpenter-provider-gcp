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
	"bytes"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	e2eci "github.com/cloudpilot-ai/karpenter-provider-gcp/hack/e2e-ci"
)

// Build this command from trusted workflow source, never from a PR bundle.
func newRenderCommand() *cobra.Command {
	var sourcePath, dir string
	command := &cobra.Command{Use: "render", Short: "Offline rendering of private runtime evidence using independently trusted gate metadata", Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error {
		// Never retain an earlier or PR-supplied report if validation fails.
		if err := os.Remove(filepath.Join(dir, "report.md")); err != nil && !os.IsNotExist(err) {
			return err
		}
		f, err := os.Open(sourcePath)
		if err != nil {
			return err
		}
		var source e2eci.Frozen
		err = e2eci.DecodeStrict(f, e2eci.MaxMetadataBytes, &source)
		f.Close()
		if err != nil {
			return err
		}
		data, err := readBoundedRegular(filepath.Join(dir, "outcome.json"), e2eci.MaxMetadataBytes)
		if err != nil {
			return err
		}
		var outcome runOutcome
		if err := e2eci.DecodeStrict(bytes.NewReader(data), e2eci.MaxMetadataBytes, &outcome); err != nil {
			return err
		}
		if err := outcome.validate(source); err != nil {
			return err
		}
		return renderPreparedReport(dir, outcome)
	}}
	command.Flags().StringVar(&sourcePath, "source", "", "Independent trusted gate JSON (not PR artifact metadata)")
	command.Flags().StringVar(&dir, "results-dir", "", "Private directory containing outcome.json and native reports")
	_ = command.MarkFlagRequired("source")
	_ = command.MarkFlagRequired("results-dir")
	return command
}

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
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	e2eci "github.com/cloudpilot-ai/karpenter-provider-gcp/hack/e2e-ci"
)

func integrationCommand(name string) bool {
	switch name {
	case "config", "pending", "publish", "download", "report-download":
		return true
	}
	return false
}
func runIntegration(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	var source, output, digest, parent, markdown, conclusion, preset, build, runtime, upload, report string
	var id int64
	switch args[0] {
	case "config":
		fs.StringVar(&preset, "preset", "", "requested preset")
	case "pending":
	case "download":
		fs.StringVar(&output, "output", "", "new original ZIP file")
		fs.Int64Var(&id, "id", 0, "artifact ID")
	case "report-download":
		fs.StringVar(&output, "output", "", "new original ZIP file")
		fs.Int64Var(&id, "id", 0, "artifact ID")
		fs.StringVar(&digest, "digest", "", "server ZIP digest")
		fs.StringVar(&parent, "destination", "", "new safe report directory")
	case "publish":
		fs.Int64Var(&id, "id", 0, "pending check ID")
		fs.StringVar(&markdown, "markdown", "", "trusted freshly rendered Markdown file (never artifact Markdown)")
		fs.StringVar(&conclusion, "conclusion", "failure", "trusted exporter verdict")
		fs.StringVar(&build, "build-status", "", "build job result")
		fs.StringVar(&runtime, "runtime-status", "", "runtime job result")
		fs.StringVar(&upload, "upload-status", "", "runtime safe upload step outcome")
		fs.StringVar(&report, "report-status", "", "report download/validation/export outcome")
	}
	if args[0] != "config" && args[0] != "download" {
		fs.StringVar(&source, "gate", "", "independent trusted gate JSON")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	encode := func(v any) error { return json.NewEncoder(out).Encode(v) }
	g := e2eci.Gate{Token: os.Getenv("GH_TOKEN"), Client: &http.Client{Timeout: 2 * time.Minute}}
	if args[0] == "config" {
		c := e2eci.Configuration{Enabled: os.Getenv("E2E_CI_ENABLED") == "true", Presets: strings.Split(os.Getenv("E2E_ENABLED_PRESETS"), ","), Workers: 20, Provider: os.Getenv("E2E_WIF_PROVIDER"), Audience: os.Getenv("E2E_WIF_AUDIENCE"), ServiceAccount: os.Getenv("E2E_RUNTIME_SERVICE_ACCOUNT")}
		if err := e2eci.DecodeStrict(strings.NewReader(os.Getenv("E2E_TARGET_CONFIG")), e2eci.MaxMetadataBytes, &c.Target); err != nil {
			return err
		}
		if err := c.Validate(preset); err != nil {
			return err
		}
		return encode(c)
	}
	if args[0] == "download" {
		if output == "" {
			return errors.New("output required")
		}
		return g.Download(ctx, id, output)
	}
	var f e2eci.Frozen
	if err := readJSON(source, &f); err != nil {
		return err
	}
	if err := f.Validate(); err != nil {
		return err
	}
	switch args[0] {
	case "pending":
		checkID, err := g.Pending(ctx, f)
		if err != nil {
			return err
		}
		return encode(map[string]int64{"id": checkID})
	case "report-download":
		if output == "" || parent == "" {
			return errors.New("output/destination required")
		}
		if err := g.ConfirmReport(ctx, f, id, digest); err != nil {
			return err
		}
		if err := g.Download(ctx, id, output); err != nil {
			return err
		}
		if err := e2eci.VerifyReportDigest(output, digest); err != nil {
			return err
		}
		return e2eci.ExtractPublicReport(output, parent)
	case "publish":
		for _, s := range []string{build, runtime, upload, report} {
			switch s {
			case "success", "failure", "cancelled", "skipped", "":
			default:
				return errors.New("invalid job/step status")
			}
		}
		text := "Native report unavailable; missing evidence cannot pass."
		if markdown != "" && report == "success" {
			file, err := os.Open(markdown)
			if err != nil {
				return err
			}
			data, readErr := io.ReadAll(io.LimitReader(file, 60001))
			closeErr := file.Close()
			if err := errors.Join(readErr, closeErr); err != nil {
				return err
			}
			if len(data) > 60000 {
				return errors.New("rendered report too large")
			}
			text = string(data)
		}
		if build != "success" || runtime != "success" || upload != "success" || report != "success" || markdown == "" {
			conclusion = "failure"
		}
		if build == "cancelled" || runtime == "cancelled" {
			conclusion = "cancelled"
		}
		text += fmt.Sprintf("\n\nDelivery: build=%s; runtime=%s; artifact upload=%s; report validation=%s. [Artifacts](%s#artifacts)\n", build, runtime, upload, report, e2eci.RunURL(f))
		if id == 0 {
			var err error
			id, err = g.Pending(ctx, f)
			if err != nil {
				_ = encode(e2eci.Delivery{Check: "failed", Comment: "unavailable"})
				return errors.New("pending check creation failed")
			}
		}
		result := g.Publish(ctx, f, id, conclusion, text)
		if err := encode(struct {
			e2eci.Delivery
			Build      string `json:"build"`
			Runtime    string `json:"runtime"`
			Upload     string `json:"artifact_upload"`
			Report     string `json:"report_validation"`
			Conclusion string `json:"conclusion"`
		}{result, build, runtime, upload, report, conclusion}); err != nil {
			return err
		}
		if result.Check == "failed" || result.Comment == "failed" {
			return errors.New("GitHub publication failed; see independent delivery outcome")
		}
		return nil
	}
	return nil
}

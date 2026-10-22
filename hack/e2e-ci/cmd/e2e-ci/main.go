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

// e2e-ci is built from the trusted workflow SHA, never from the PR checkout.
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
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	e2eci "github.com/cloudpilot-ai/karpenter-provider-gcp/hack/e2e-ci"
)

func dispatch(pr, preset string) (e2eci.Dispatch, error) {
	id, err := strconv.ParseInt(os.Getenv("GITHUB_ACTOR_ID"), 10, 64)
	if err != nil {
		return e2eci.Dispatch{}, err
	}
	run, err := strconv.ParseInt(os.Getenv("GITHUB_RUN_ID"), 10, 64)
	if err != nil {
		return e2eci.Dispatch{}, err
	}
	attempt, err := strconv.Atoi(os.Getenv("GITHUB_RUN_ATTEMPT"))
	if err != nil {
		return e2eci.Dispatch{}, err
	}
	return e2eci.Dispatch{Repository: os.Getenv("GITHUB_REPOSITORY"), Ref: os.Getenv("GITHUB_REF"), WorkflowRef: os.Getenv("GITHUB_WORKFLOW_REF"), WorkflowSHA: os.Getenv("GITHUB_WORKFLOW_SHA"), Event: os.Getenv("GITHUB_EVENT_NAME"), Actor: os.Getenv("GITHUB_ACTOR"), ActorID: id, RunID: run, RunAttempt: attempt, PR: pr, Preset: preset}, nil
}
func readJSON(file string, v any) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	return e2eci.DecodeStrict(f, e2eci.MaxMetadataBytes, v)
}
func run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("expected gate, recheck, fetch, prepare, artifact or verify")
	}
	if integrationCommand(args[0]) {
		return runIntegration(ctx, args, out)
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	var pr, preset, enabled, gatePath, receiptPath, source, destination, output, digest, zipPath, parent string
	var artifactID int64
	switch args[0] {
	case "gate", "recheck":
		fs.StringVar(&pr, "pr", "", "positive PR number")
		fs.StringVar(&preset, "preset", "", "standard|gpu|full")
		fs.StringVar(&enabled, "enabled-presets", "", "trusted administrator enabled comma-separated presets; empty disables all")
	case "fetch":
		fs.StringVar(&destination, "destination", "", "new isolated PR checkout directory")
	case "prepare":
		fs.StringVar(&source, "source", "", "frozen PR checkout")
		fs.StringVar(&output, "output", "", "new bundle.tar outside checkout")
	case "artifact":
		fs.Int64Var(&artifactID, "id", 0, "exact artifact ID from this invocation upload")
		fs.StringVar(&digest, "digest", "", "server sha256 digest from this invocation upload")
	case "verify":
		fs.StringVar(&receiptPath, "receipt", "", "trusted artifact receipt JSON")
		fs.StringVar(&zipPath, "zip", "", "downloaded artifact ZIP")
		fs.StringVar(&parent, "parent", "", "private extraction parent directory")
	default:
		return errors.New("unknown command")
	}
	if args[0] != "gate" {
		fs.StringVar(&gatePath, "gate", "", "trusted gate JSON (never downloaded from build artifact)")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	gate := e2eci.Gate{Token: os.Getenv("GH_TOKEN"), Client: &http.Client{Timeout: 30 * time.Second}}
	var frozen e2eci.Frozen
	if args[0] != "gate" {
		if err := readJSON(gatePath, &frozen); err != nil {
			return err
		}
		if err := frozen.Validate(); err != nil {
			return err
		}
	}
	encode := func(v any) error { return json.NewEncoder(out).Encode(v) }
	switch args[0] {
	case "gate":
		d, err := dispatch(pr, preset)
		if err != nil {
			return err
		}
		f, err := gate.Capture(ctx, d, strings.Split(enabled, ","))
		if err != nil {
			return err
		}
		return encode(f)
	case "recheck":
		d, err := dispatch(pr, preset)
		if err != nil {
			return err
		}
		return gate.Recheck(ctx, frozen, d, strings.Split(enabled, ","))
	case "fetch":
		if destination == "" {
			return errors.New("--destination required")
		}
		return e2eci.FetchSource(ctx, destination, frozen)
	case "prepare":
		if source == "" || output == "" {
			return errors.New("--source and --output required")
		}
		return e2eci.Prepare(ctx, source, output, frozen)
	case "artifact":
		r, err := gate.Artifact(ctx, frozen, artifactID, digest)
		if err != nil {
			return err
		}
		return encode(r)
	case "verify":
		if parent == "" {
			return errors.New("--parent required")
		}
		var r e2eci.Receipt
		if err := readJSON(receiptPath, &r); err != nil {
			return err
		}
		b, err := e2eci.VerifyDownload(zipPath, parent, frozen, r)
		if err != nil {
			return err
		}
		return encode(b)
	}
	return nil
}
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

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
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	e2eci "github.com/cloudpilot-ai/karpenter-provider-gcp/hack/e2e-ci"
)

func TestGateCLIRejectsForkBeforeNetwork(t *testing.T) {
	for key, value := range map[string]string{"GITHUB_REPOSITORY": "contributor/fork", "GITHUB_REF": "refs/heads/main", "GITHUB_WORKFLOW_REF": e2eci.Repository + "/" + e2eci.WorkflowPath + "@refs/heads/main", "GITHUB_WORKFLOW_SHA": strings.Repeat("1", 40), "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_ACTOR": "maintainer", "GITHUB_ACTOR_ID": "7", "GITHUB_RUN_ID": "42", "GITHUB_RUN_ATTEMPT": "1"} {
		t.Setenv(key, value)
	}
	var out bytes.Buffer
	err := run(context.Background(), []string{"gate", "--pr=12", "--preset=standard", "--enabled-presets=standard"}, &out)
	if err == nil || !strings.Contains(err.Error(), "upstream main") || out.Len() != 0 {
		t.Fatalf("unexpected CLI authorization: %v %s", err, out.String())
	}
}
func TestCLIFailsClosedForUnknownAndIncompleteCommands(t *testing.T) {
	for _, args := range [][]string{nil, {"shell"}, {"gate", "--arbitrary-script=x"}, {"prepare"}, {"verify"}, {"artifact"}, {"gate", "unexpected"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if err := run(context.Background(), args, new(bytes.Buffer)); err == nil {
				t.Fatal("incomplete command accepted")
			}
		})
	}
}
func TestPrepareCLIConsumesTrustedGateFile(t *testing.T) {
	f := e2eci.Frozen{Schema: 1, BaseRepositoryID: e2eci.RepositoryID, PRNumber: 12, HeadRepositoryID: 99, HeadRepository: "contributor/fork", HeadSHA: strings.Repeat("2", 40), Dispatch: e2eci.Dispatch{Repository: e2eci.Repository, Ref: "refs/heads/main", WorkflowRef: e2eci.Repository + "/" + e2eci.WorkflowPath + "@refs/heads/main", WorkflowSHA: strings.Repeat("1", 40), Event: "workflow_dispatch", Actor: "maintainer", ActorID: 7, RunID: 42, RunAttempt: 1, PR: "12", Preset: "standard"}}
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "gate.json")
	os.WriteFile(file, data, 0600)
	err = run(context.Background(), []string{"prepare", "--gate=" + file}, new(bytes.Buffer))
	if err == nil || !strings.Contains(err.Error(), "--source and --output") {
		t.Fatalf("CLI did not consume valid gate file: %v", err)
	}
	os.WriteFile(file, append(data, []byte("{}")...), 0600)
	if err := run(context.Background(), []string{"prepare", "--gate=" + file}, new(bytes.Buffer)); err == nil || !strings.Contains(err.Error(), "trailing metadata") {
		t.Fatalf("trailing metadata accepted: %v", err)
	}
}

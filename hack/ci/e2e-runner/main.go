/*
Copyright 2026 The CloudPilot AI Authors.

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

// The bootstrap runner advertises the stable bridge without executing e2e suites.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

func main() {
	if err := execute(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func execute(args []string) error {
	if len(args) == 1 && args[0] == "capabilities" {
		fmt.Println(`{"version":1,"commands":["capabilities","prepare","run","report"],"modes":{"standard":"not_implemented"}}`)
		return nil
	}
	if len(args) == 0 || (args[0] != "prepare" && args[0] != "run" && args[0] != "report") {
		return fmt.Errorf("unsupported command")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	invocationPath := flags.String("invocation", "", "captured invocation JSON")
	output := flags.String("output", "", "result JSON path")
	flags.String("source", "", "code-under-test directory (prepare only)")
	flags.String("bundle", "", "prepared bundle directory")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *invocationPath == "" || *output == "" {
		return fmt.Errorf("invocation and output are required")
	}
	stat, err := os.Stat(*invocationPath)
	if err != nil || stat.Size() > 1024*1024 {
		return fmt.Errorf("invalid invocation file")
	}
	data, err := os.ReadFile(*invocationPath)
	if err != nil {
		return err
	}
	var invocation struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &invocation); err != nil || invocation.Version != 1 {
		return fmt.Errorf("incompatible invocation")
	}
	result, err := json.Marshal(struct {
		Version    int             `json:"version"`
		Invocation json.RawMessage `json:"invocation"`
		Status     string          `json:"status"`
		Executed   int             `json:"executed"`
	}{1, data, "not_implemented", 0})
	if err != nil {
		return err
	}
	return os.WriteFile(*output, append(result, '\n'), 0600)
}

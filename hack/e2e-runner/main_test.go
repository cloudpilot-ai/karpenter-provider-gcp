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
	"testing"

	"github.com/onsi/ginkgo/v2/types"
)

func TestLocalSSDSelection(t *testing.T) {
	for _, tc := range []struct {
		selection string
		basic     bool
		extended  bool
	}{
		{selection: "local-ssd", basic: true},
		{selection: "local-ssd-extended", extended: true},
		{selection: "local-ssd,local-ssd-extended", basic: true, extended: true},
		{selection: "all", basic: true, extended: true},
	} {
		t.Run(tc.selection, func(t *testing.T) {
			filter, err := selectFilter("../../test/suites", tc.selection)
			if err != nil {
				t.Fatal(err)
			}
			matches, err := types.ParseLabelFilter(filter)
			if err != nil {
				t.Fatal(err)
			}
			for suite, want := range map[string]bool{
				"local-ssd":          tc.basic,
				"local-ssd-extended": tc.extended,
			} {
				if got := matches([]string{"suite:" + suite}); got != want {
					t.Fatalf("%s selection matches %s = %v, want %v", tc.selection, suite, got, want)
				}
			}
		})
	}
}

func TestStandardSelection(t *testing.T) {
	filter, err := selectFilter("../../test/suites", "standard")
	if err != nil {
		t.Fatal(err)
	}
	matches, err := types.ParseLabelFilter(filter)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		suite string
		want  bool
	}{
		{suite: "gpu", want: false},
		{suite: "local-ssd-extended", want: false},
		{suite: "local-ssd", want: true},
		{suite: "storage", want: true},
		{suite: "provisioning", want: true},
	} {
		t.Run(tc.suite, func(t *testing.T) {
			if got := matches([]string{"suite:" + tc.suite}); got != tc.want {
				t.Fatalf("standard selection matches %s = %v, want %v", tc.suite, got, tc.want)
			}
		})
	}
}

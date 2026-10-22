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

package native_test

import (
	. "github.com/onsi/ginkgo/v2"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestNative(t *testing.T) { RunSpecs(t, "Prepared native fixture") }

var _ = Describe("prepared fixture", func() {
	It("executes standard", Label("suite:storage"), func() {
		config, _ := GinkgoConfiguration()
		if config.ParallelTotal != 20 {
			Fail("global workers not 20")
		}
		if os.Getenv("GITHUB_TOKEN") != "" {
			Fail("GitHub token leaked")
		}
		callback := os.Getenv("PROJECT_ID")
		if strings.HasPrefix(os.Getenv("RESOURCE_MANAGER_TAG_KEY"), "http://") {
			callback = os.Getenv("RESOURCE_MANAGER_TAG_KEY")
		}
		if strings.HasPrefix(callback, "http://") {
			response, err := http.Get(callback)
			if err != nil {
				Fail(err.Error())
			}
			response.Body.Close()
			select {}
		}
		if os.Getenv("PROJECT_ID") == "fixture-fail" {
			Fail("fixture failure")
		}
	})
	It("executes gpu", Label("suite:gpu"), func() {})
	It("executes extended SSD", Label("suite:local-ssd-extended"), func() {})
})

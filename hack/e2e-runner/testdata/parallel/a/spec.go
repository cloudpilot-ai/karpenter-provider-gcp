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

package a

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
)

var _ = Describe("A", Label("suite:a"), func() {
	It("first", func() { time.Sleep(200 * time.Millisecond) })
	It("second", func() { time.Sleep(200 * time.Millisecond) })
})

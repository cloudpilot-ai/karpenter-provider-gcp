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

// Package instance owns GCE VM lifecycle operations for Karpenter NodeClaims.
//
// It selects offerings, creates and deletes Compute Engine instances, maps GCE
// instances back to Karpenter instances, and owns provider-managed instance
// fields such as labels, tags, scheduling, capacity errors, and zone operation
// handling. Bootstrap source discovery and pure bootstrap metadata mutation live
// in nodepooltemplate and metadata respectively.
package instance

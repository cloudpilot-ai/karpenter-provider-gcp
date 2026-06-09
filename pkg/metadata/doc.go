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

// Package metadata defines the provider's typed model for GKE bootstrap
// metadata and GCE instance labels.
//
// This package is a pure modeling and transformation layer. It must not perform
// GCP API calls, discover source node pools or instance templates, create GCE
// instances, or decide scheduling/ownership policy. Callers provide source
// template metadata and provider-owned target values; this package parses only
// the explicitly supported source fields, applies typed updates, and serializes
// the result back to Compute Engine shapes.
//
// Source-template metadata is not treated as an arbitrary passthrough bag. New
// source fields may be retained only by adding typed fields or explicitly named
// copy rules here. This keeps reviews focused on which metadata surfaces are
// inherited from GKE and which are fully owned by the instance provider.
package metadata

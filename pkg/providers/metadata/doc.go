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

// Package metadata provides pure transformations for GKE bootstrap metadata.
//
// It edits kube-env, kubelet configuration, startup script, label, and taint
// values before callers convert them back to Compute Engine metadata items. It
// should not perform GCP API calls or own Kubernetes object metadata, GCE
// instance labels, or instance tags.
package metadata

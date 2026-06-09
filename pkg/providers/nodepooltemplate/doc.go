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

// Package nodepooltemplate selects and maintains the GKE node pool used as the
// bootstrap source for Karpenter-created nodes.
//
// It discovers eligible source pools, creates the fallback source pool when
// needed, and reads the GKE-managed instance template metadata needed by the VM
// creation path. It should express source-pool policy rather than own GCE VM
// lifecycle, instance labels, or generic bootstrap metadata transformations.
package nodepooltemplate

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

package metadata

import (
	"sort"

	"github.com/samber/lo"
	"google.golang.org/api/compute/v1"
)

type InstanceMetadata map[string]string

func FromAPI(api *compute.Metadata) InstanceMetadata {
	m := InstanceMetadata{}
	if api == nil {
		return m
	}
	for _, item := range api.Items {
		if item == nil || item.Key == "" {
			continue
		}
		m[item.Key] = lo.FromPtr(item.Value)
	}
	return m
}

func (m InstanceMetadata) ToAPI() *compute.Metadata {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	items := make([]*compute.MetadataItems, 0, len(keys))
	for _, key := range keys {
		items = append(items, &compute.MetadataItems{Key: key, Value: lo.ToPtr(m[key])})
	}
	return &compute.Metadata{Items: items}
}

func (m InstanceMetadata) MergeUser(user map[string]string, reserved map[string]struct{}) {
	if m == nil {
		return
	}
	for key, value := range user {
		if value == "" {
			continue
		}
		if _, ok := reserved[key]; ok {
			continue
		}
		m[key] = value
	}
}

func ReplaceAPI(target *compute.Metadata, m InstanceMetadata) {
	if target == nil {
		return
	}
	target.Items = m.ToAPI().Items
}

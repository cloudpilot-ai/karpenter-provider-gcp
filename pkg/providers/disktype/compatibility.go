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

package disktype

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const labelPrefix = "disk-type.gke.io/"

//go:embed pdcsi/node-labeler-configmap.yaml
var upstreamConfigMap embed.FS

type configMap struct {
	Data map[string]string `yaml:"data"`
}

var compatibility = mustLoadCompatibility()

func mustLoadCompatibility() map[string][]string {
	contents, err := upstreamConfigMap.ReadFile("pdcsi/node-labeler-configmap.yaml")
	if err != nil {
		panic(fmt.Sprintf("reading vendored PDCSI compatibility configmap: %v", err))
	}

	var cm configMap
	if err := yaml.Unmarshal(contents, &cm); err != nil {
		panic(fmt.Sprintf("parsing vendored PDCSI compatibility configmap: %v", err))
	}

	jsonText := cm.Data["machine-pd-compatibility.json"]
	if jsonText == "" {
		panic("vendored PDCSI compatibility configmap missing machine-pd-compatibility.json")
	}

	var raw map[string]map[string]bool
	if err := json.Unmarshal([]byte(jsonText), &raw); err != nil {
		panic(fmt.Sprintf("parsing machine-pd-compatibility.json: %v", err))
	}

	out := make(map[string][]string, len(raw))
	for family, diskTypes := range raw {
		labels := make([]string, 0, len(diskTypes))
		for diskType, supported := range diskTypes {
			if supported {
				labels = append(labels, labelPrefix+diskType)
			}
		}
		sort.Strings(labels)
		out[family] = labels
	}
	return out
}

// Family returns the GCE machine family prefix from an instance type name.
func Family(instanceType string) string {
	if instanceType == "" {
		return ""
	}
	family, _, _ := strings.Cut(instanceType, "-")
	return family
}

// LabelsForInstanceType returns disk-type.gke.io labels for the instance type's family.
func LabelsForInstanceType(instanceType string) (map[string]string, bool) {
	return LabelsForFamily(Family(instanceType))
}

// LabelsForFamily returns disk-type.gke.io labels for a GCE machine family.
func LabelsForFamily(family string) (map[string]string, bool) {
	labels, ok := compatibility[family]
	if !ok {
		return nil, false
	}
	out := make(map[string]string, len(labels))
	for _, label := range labels {
		out[label] = "true"
	}
	return out, true
}

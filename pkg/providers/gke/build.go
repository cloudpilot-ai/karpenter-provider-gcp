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

package gke

import (
	"fmt"
	"regexp"
	"strconv"
)

// gkeReleasePattern matches the GKE release versions that GKE writes into its
// bootstrap archive URLs, such as v1.34.1-gke.1341000. A release without a GKE
// build suffix, such as v1.34.7 from a channel version list, has build 0.
var gkeReleasePattern = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-gke\.(\d+))?$`)

type gkeRelease struct {
	major, minor, patch, build int64
}

// CompareGKEReleases orders two GKE release versions, returning a negative
// number when a is older, 0 when they are equal and a positive number when a is
// newer. The Kubernetes version is compared first and the GKE build number
// after it, as an integer: v1.34.1-gke.999 is older than v1.34.1-gke.1341000,
// which string ordering gets wrong.
func CompareGKEReleases(a, b string) (int, error) {
	left, err := parseGKERelease(a)
	if err != nil {
		return 0, err
	}
	right, err := parseGKERelease(b)
	if err != nil {
		return 0, err
	}
	if c := compareInt64(left.major, right.major); c != 0 {
		return c, nil
	}
	if c := compareInt64(left.minor, right.minor); c != 0 {
		return c, nil
	}
	if c := compareInt64(left.patch, right.patch); c != 0 {
		return c, nil
	}
	return compareInt64(left.build, right.build), nil
}

// GKEReleaseAtLeast reports whether version is minimum or newer. An
// unrecognizable version returns an error rather than a default answer, because
// callers use it to decide whether a GKE bootstrap supports a feature.
func GKEReleaseAtLeast(version, minimum string) (bool, error) {
	order, err := CompareGKEReleases(version, minimum)
	if err != nil {
		return false, err
	}
	return order >= 0, nil
}

func parseGKERelease(version string) (gkeRelease, error) {
	match := gkeReleasePattern.FindStringSubmatch(version)
	if match == nil {
		return gkeRelease{}, fmt.Errorf("unknown GKE release version %q", version)
	}
	parts := make([]int64, 4)
	for i, raw := range match[1:] {
		if raw == "" {
			continue
		}
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return gkeRelease{}, fmt.Errorf("unknown GKE release version %q: %w", version, err)
		}
		parts[i] = value
	}
	return gkeRelease{major: parts[0], minor: parts[1], patch: parts[2], build: parts[3]}, nil
}

func compareInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

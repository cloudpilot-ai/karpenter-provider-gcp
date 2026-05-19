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

package gke

import (
	"fmt"
	"sort"
	"strings"

	containerv1 "google.golang.org/api/container/v1"
	k8sversion "k8s.io/apimachinery/pkg/util/version"
)

// ResolveVersionForChannel returns the GKE version string for channelName that matches
// the minor version of clusterVersion. channelName is normalised to uppercase.
//
// Algorithm (proposal/0002 §Version Resolution Algorithm):
//
//	Step 1: if defaultVersion minor == clusterVersion minor → return defaultVersion.
//	Step 2: filter validVersions by minor using semver; return the highest.
//	Step 3: no match → return an error (no silent fallback).
func ResolveVersionForChannel(sc *containerv1.ServerConfig, channelName, clusterVersion string) (string, error) {
	channelName = strings.ToUpper(channelName)

	ch := findChannelConfig(sc, channelName)
	if ch == nil {
		return "", fmt.Errorf("channel %s not found in getServerConfig response", channelName)
	}

	clusterMinor, err := extractMinorVersion(clusterVersion)
	if err != nil {
		return "", fmt.Errorf("parsing cluster version %q: %w", clusterVersion, err)
	}

	// Step 1: defaultVersion minor matches.
	if ch.DefaultVersion != "" {
		if m, err := extractMinorVersion(ch.DefaultVersion); err == nil && m == clusterMinor {
			return ch.DefaultVersion, nil
		}
	}

	// Step 2: highest validVersion matching clusterMinor.
	if v := highestVersionForMinor(ch.ValidVersions, clusterMinor); v != "" {
		return v, nil
	}

	return "", fmt.Errorf("channel %s has no valid version for cluster minor %s; "+
		"the requested channel may not yet support this Kubernetes minor — "+
		"switch to a channel that does, or use version: latest explicitly", channelName, clusterMinor)
}

func findChannelConfig(sc *containerv1.ServerConfig, channelName string) *containerv1.ReleaseChannelConfig {
	for _, c := range sc.Channels {
		if c.Channel == channelName {
			return c
		}
	}
	return nil
}

// highestVersionForMinor returns the semver-highest entry in versions whose major.minor
// equals minor, or "" if no match is found.
func highestVersionForMinor(versions []string, minor string) string {
	var candidates []string
	for _, v := range versions {
		if m, err := extractMinorVersion(v); err == nil && m == minor {
			candidates = append(candidates, v)
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	sort.Slice(candidates, func(i, j int) bool {
		vi, erri := k8sversion.ParseGeneric(candidates[i])
		if erri != nil {
			return candidates[i] > candidates[j]
		}
		cmp, err := vi.Compare(candidates[j])
		if err != nil {
			return candidates[i] > candidates[j]
		}
		return cmp > 0
	})
	return candidates[0]
}

// extractMinorVersion returns "major.minor" from a version string.
// It uses k8sversion.ParseGeneric so that "v1.34.7" and "1.34.6-gke.1068000"
// both normalise to "1.34".
func extractMinorVersion(v string) (string, error) {
	parsed, err := k8sversion.ParseGeneric(v)
	if err != nil {
		return "", fmt.Errorf("cannot parse version %q: %w", v, err)
	}
	return fmt.Sprintf("%d.%d", parsed.Major(), parsed.Minor()), nil
}

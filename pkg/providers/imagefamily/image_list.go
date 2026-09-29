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

package imagefamily

import (
	"context"

	"google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
)

// scanImages visits images newest first and stops when visit returns true.
// Only the current page is retained; server-side filtering is not used.
func scanImages(ctx context.Context, service *compute.Service, project string, visit func(*compute.Image) bool) error {
	pageToken := ""
	for {
		call := service.Images.List(project).
			OrderBy("creationTimestamp desc").
			Fields(googleapi.Field("nextPageToken,items(name,creationTimestamp,status,deprecated/state)"))
		if pageToken != "" {
			call.PageToken(pageToken)
		}
		page, err := call.Context(ctx).Do()
		if err != nil {
			return err
		}
		for _, image := range page.Items {
			if visit(image) {
				return nil
			}
		}
		if page.NextPageToken == "" {
			return nil
		}
		pageToken = page.NextPageToken
	}
}

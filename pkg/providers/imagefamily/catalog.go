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
	"errors"
	"math/rand"
	"strings"
	"sync"
	"time"

	"google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type catalogRateLimitError struct {
	cause      error
	retryAfter time.Duration
}

func (e *catalogRateLimitError) Error() string { return e.cause.Error() }
func (e *catalogRateLimitError) Unwrap() error { return e.cause }

// CatalogRateLimitRetryAfter reports only structured catalog-list quota errors.
func CatalogRateLimitRetryAfter(err error) (time.Duration, bool) {
	var quotaErr *catalogRateLimitError
	if !errors.As(err, &quotaErr) {
		return 0, false
	}
	return quotaErr.retryAfter, true
}

func isCatalogQuotaError(err error) bool {
	var apiErr *googleapi.Error
	if !errors.As(err, &apiErr) || apiErr.Code != 403 {
		return false
	}
	for _, item := range apiErr.Errors {
		if strings.EqualFold(item.Reason, "rateLimitExceeded") || item.Reason == "RATE_LIMIT_EXCEEDED" {
			return true
		}
	}
	return false
}

// catalogCooldown gates subsequent catalog scans across selector variants within one process.
type catalogCooldown struct {
	sync.Mutex
	until map[string]time.Time
	now   func() time.Time
}

func (c *catalogCooldown) remaining(project string) time.Duration {
	c.Lock()
	defer c.Unlock()
	now := c.now()
	if until := c.until[project]; until.After(now) {
		return until.Sub(now)
	}
	return 0
}

func (c *catalogCooldown) rateLimited(project string) time.Duration {
	c.Lock()
	defer c.Unlock()
	// Allow the quota bucket to refill; jitter prevents synchronized replicas.
	delay := time.Minute + time.Duration(rand.Int63n(int64(10*time.Second))) //nolint:gosec // Retry jitter needs no cryptographic entropy.
	if c.until == nil {
		c.until = make(map[string]time.Time)
	}
	c.until[project] = c.now().Add(delay)
	return delay
}

// scanImageCatalog visits images newest first and stops when visit returns true.
// Only the current page is retained; the quota-costly server-side filter is not used.
func scanImageCatalog(ctx context.Context, service *compute.Service, project string, cooldown *catalogCooldown, visit func(*compute.Image) bool) error {
	if cooldown != nil {
		if remaining := cooldown.remaining(project); remaining > 0 {
			return &catalogRateLimitError{cause: errors.New("image catalog rate limit cooldown"), retryAfter: remaining}
		}
	}
	start := time.Now()
	pages := 0
	earlyStop := false
	defer func() {
		log.FromContext(ctx).V(1).Info("scanned image catalog", "project", project, "pages", pages, "earlyStop", earlyStop, "duration", time.Since(start))
	}()

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
			if isCatalogQuotaError(err) {
				delay := time.Minute
				if cooldown != nil {
					delay = cooldown.rateLimited(project)
				}
				return &catalogRateLimitError{cause: err, retryAfter: delay}
			}
			return err
		}
		pages++
		for _, image := range page.Items {
			if visit(image) {
				earlyStop = true
				return nil
			}
		}
		if page.NextPageToken == "" {
			return nil
		}
		pageToken = page.NextPageToken
	}
}

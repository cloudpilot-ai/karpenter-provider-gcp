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

package serviceaccount

import (
	"context"
	"fmt"
	"sync"
	"time"

	"google.golang.org/api/compute/v1"
	"k8s.io/utils/clock"
)

const (
	lookupTimeout = 3 * time.Second
	retryInterval = 10 * time.Second
)

type Provider interface {
	Resolve(context.Context, string) (string, error)
}

type DefaultProvider struct {
	client         *compute.Service
	projectID      string
	defaultAccount string
	clock          clock.PassiveClock

	mu             sync.Mutex
	projectAccount string
	retryAfter     time.Time
	lastError      error
}

// NewProvider resolves the controller default before startup can proceed.
// An empty project default is allowed: NodeClasses may supply their own accounts.
func NewProvider(ctx context.Context, client *compute.Service, projectID, defaultAccount string, clock clock.PassiveClock) (Provider, error) {
	p := &DefaultProvider{client: client, projectID: projectID, defaultAccount: defaultAccount, clock: clock}
	if _, err := p.Resolve(ctx, ""); err != nil {
		return nil, err
	}
	return p, nil
}

// Resolve uses the explicit account, controller default, then project default.
// Only successful nonempty project defaults are cached for the process lifetime.
func (p *DefaultProvider) Resolve(ctx context.Context, explicitAccount string) (string, error) {
	if explicitAccount != "" {
		return explicitAccount, nil
	}
	if p.defaultAccount != "" {
		return p.defaultAccount, nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if p.projectAccount != "" {
		return p.projectAccount, nil
	}
	if p.clock.Now().Before(p.retryAfter) {
		return "", p.lastError
	}
	lookupCtx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	project, err := p.client.Projects.Get(p.projectID).Context(lookupCtx).Do()
	if err != nil {
		err = fmt.Errorf("getting Compute Engine default service account: %w", err)
	} else {
		p.projectAccount = project.DefaultServiceAccount
	}
	// A canceled caller must not delay other callers' recovery.
	if ctx.Err() == nil {
		p.lastError = err
		p.retryAfter = p.clock.Now().Add(retryInterval)
	}
	return p.projectAccount, err
}

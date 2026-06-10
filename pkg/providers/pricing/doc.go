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

// Package pricing provides regional on-demand and Spot prices for GCE machine
// types.
//
// It owns embedded fallback prices, live Cloud Billing API refreshes, and price
// lookup state consumed by instance-type discovery. It should not apply pricing
// decisions to NodeClaims directly.
package pricing

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

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/iterator"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/pricing/instanceprice"
)

var knownExtras = map[string]bool{
	"a3-edgegpu-8g":         true,
	"a3-edgegpu-8g-nolssd":  true,
	"a3-megagpu-8g":         true, // in Cyclenerd but prices differ in some regions; still absent from some regions
	"a3-ultragpu-8g":        true, // base variant not yet in reference sources in all regions
	"a3-ultragpu-8g-nolssd": true,
}

func main() {
	filterRegion := flag.String("region", "all", `Region to compare, e.g. "us-central1", or "all"`)
	tolerance := flag.Float64("tolerance", 0.01, "Max allowed fractional price difference (default 1%)")
	noCache := flag.Bool("no-cache", false, "Ignore all caches and fetch everything fresh")
	workDir := flag.String("work-dir", "./data", "Directory for cache and output files")
	cacheTTL := flag.Duration("cache-ttl", 12*time.Hour, "Max age of reference price caches before re-fetching")
	project := flag.String("project", "", "GCP project ID (auto-detected from $GOOGLE_CLOUD_PROJECT or ADC if unset)")
	flag.Parse()

	if err := os.MkdirAll(*workDir, 0755); err != nil {
		log.Fatalf("creating work directory %s: %v", *workDir, err)
	}

	ctx := context.Background()

	fmt.Println("Phase 1: Reference prices")
	var (
		cyclenerdPrices RegionPrices
		gcpWebPrices    RegionPrices
		cycErr, gcpErr  error
		wg              sync.WaitGroup
	)
	wg.Add(2)
	go func() { defer wg.Done(); cyclenerdPrices, cycErr = getCyclenerdPrices(ctx, *workDir, *noCache, *cacheTTL) }()
	go func() { defer wg.Done(); gcpWebPrices, gcpErr = getGCPWebPrices(ctx, *workDir, *noCache, *cacheTTL) }()
	wg.Wait()
	if cycErr != nil {
		log.Fatalf("  cyclenerd prices: %v", cycErr)
	}
	if gcpErr != nil {
		log.Fatalf("  gcp web prices: %v", gcpErr)
	}

	fmt.Println("Phase 2: Computed prices (instanceprice)")
	resolvedProject, err := resolveProject(ctx, *project)
	if err != nil {
		log.Fatalf("  resolving GCP project: %v", err)
	}

	var regions []string
	if *filterRegion == "all" {
		regions, err = getGCPRegions(ctx, resolvedProject)
		if err != nil {
			log.Fatalf("  fetching GCP regions: %v", err)
		}
		fmt.Printf("  GCP reports %d regions\n", len(regions))
	} else {
		regions = []string{*filterRegion}
	}

	computedPrices, err := fetchInstancePrices(ctx, regions)
	if err != nil {
		log.Fatalf("  computed prices: %v", err)
	}
	if len(computedPrices) == 0 {
		log.Fatal("  computed prices: no data")
	}
	computedPath := filepath.Join(*workDir, "computed.json")
	// nil timestamp: omit saved_at so git diff only fires on actual price changes.
	if err := writePriceFile(computedPath, computedPrices, nil); err != nil {
		log.Fatalf("  writing %s: %v", computedPath, err)
	}

	fmt.Print("  Fetching machine type availability...")
	availability, err := getMachineAvailability(ctx, resolvedProject, regions)
	if err != nil {
		log.Fatalf("  machine availability: %v", err)
	}
	fmt.Printf(" %d regions\n", len(availability))

	fmt.Println("Phase 3: Comparing prices")
	// refs ordered by trust
	refs := []RegionPrices{gcpWebPrices, cyclenerdPrices}
	results := buildResults(computedPrices, refs, availability, regions, func(m string) bool { return knownExtras[m] }, *tolerance)
	os.Exit(printReport(results, []string{"gcp_web", "cyclenerd"}, len(regions), *tolerance))
}

func getGCPRegions(ctx context.Context, project string) ([]string, error) {
	client, err := compute.NewRegionsRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating regions client: %w", err)
	}
	defer client.Close()

	it := client.List(ctx, &computepb.ListRegionsRequest{Project: project})
	var regions []string
	for {
		region, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("listing regions: %w", err)
		}
		regions = append(regions, region.GetName())
	}
	slices.Sort(regions)
	return regions, nil
}

func resolveProject(ctx context.Context, flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	for _, env := range []string{"GOOGLE_CLOUD_PROJECT", "GCLOUD_PROJECT"} {
		if v := os.Getenv(env); v != "" {
			return v, nil
		}
	}
	if creds, err := google.FindDefaultCredentials(ctx); err == nil && creds.ProjectID != "" {
		return creds.ProjectID, nil
	}
	return "", fmt.Errorf("GCP project not found; set --project, $GOOGLE_CLOUD_PROJECT, or run: gcloud auth application-default login")
}

func fetchInstancePrices(ctx context.Context, regions []string) (RegionPrices, error) {
	client, err := instanceprice.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating instanceprice client: %w", err)
	}
	defer client.Close()

	all, err := client.FetchAllPrices(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetching prices: %w", err)
	}

	computed := make(RegionPrices, len(regions))
	for _, region := range regions {
		if prices, ok := all[region]; ok {
			computed[region] = prices
		}
	}
	fmt.Printf("  Computed prices for %d regions\n", len(computed))
	return computed, nil
}

// getMachineAvailability queries the Compute Engine aggregated machine types
// API and returns, for each region, the set of machine types that are deployed
// there. classifyMissing uses this to distinguish MISSING (machine available
// but we have no price) from UNAVAIL (machine not deployed in the region).
func getMachineAvailability(ctx context.Context, project string, regions []string) (RegionAvailability, error) {
	client, err := compute.NewMachineTypesRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating machine types client: %w", err)
	}
	defer client.Close()

	regionSet := make(map[string]bool, len(regions))
	for _, r := range regions {
		regionSet[r] = true
	}

	availability := make(RegionAvailability, len(regions))
	for _, r := range regions {
		availability[r] = make(map[string]bool)
	}

	it := client.AggregatedList(ctx, &computepb.AggregatedListMachineTypesRequest{
		Project: project,
	})
	for {
		pair, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("listing machine types: %w", err)
		}
		zone := strings.TrimPrefix(pair.Key, "zones/")
		region := zoneToRegion(zone)
		if !regionSet[region] {
			continue
		}
		for _, mt := range pair.Value.GetMachineTypes() {
			availability[region][mt.GetName()] = true
		}
	}
	return availability, nil
}

// zoneToRegion strips the trailing zone letter from a GCE zone name.
// e.g. "us-central1-a" → "us-central1".
func zoneToRegion(zone string) string {
	idx := strings.LastIndex(zone, "-")
	if idx < 0 {
		return zone
	}
	return zone[:idx]
}

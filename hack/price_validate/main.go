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
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils/localssd"
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
		gcpWebSSDRates  SSDSpotRates
		cycErr, gcpErr  error
		wg              sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		cyclenerdPrices, cycErr = getCyclenerdPrices(ctx, *workDir, *noCache, *cacheTTL)
	}()
	go func() {
		defer wg.Done()
		gcpWebPrices, gcpWebSSDRates, gcpErr = getGCPWebPrices(ctx, *workDir, *noCache, *cacheTTL)
	}()
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

	computedPrices, err := fetchInstancePrices(ctx, resolvedProject, regions)
	if err != nil {
		log.Fatalf("  computed prices: %v", err)
	}
	if len(computedPrices) == 0 {
		log.Fatal("  computed prices: no data")
	}
	computedPath := filepath.Join(*workDir, "computed.json")
	// nil timestamp: omit saved_at so git diff only fires on actual price changes.
	// Spot is omitted from the committed seed file; startup uses on-demand as the
	// conservative Spot default until live prices are fetched.
	if err := writePriceFile(computedPath, withoutSpotPrices(computedPrices), nil); err != nil {
		log.Fatalf("  writing %s: %v", computedPath, err)
	}

	fmt.Print("  Fetching machine type availability...")
	availability, scratchDiskSizes, err := getMachineAvailability(ctx, resolvedProject, regions)
	if err != nil {
		log.Fatalf("  machine availability: %v", err)
	}
	fmt.Printf(" %d regions\n", len(availability))

	if len(gcpWebSSDRates) > 0 && len(scratchDiskSizes) > 0 {
		fillLSSDSpotPrices(gcpWebPrices, gcpWebSSDRates, scratchDiskSizes)
	}

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

func fetchInstancePrices(ctx context.Context, project string, regions []string) (RegionPrices, error) {
	client, err := instanceprice.New(ctx, project)
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

// getMachineAvailability queries the Compute Engine aggregated machine types API.
// It returns the set of machine types available per region (for MISSING vs UNAVAIL
// classification) and a global map of machine type → total bundled local SSD GiB
// (for deriving -lssd spot prices from web page SSD rates).
func getMachineAvailability(ctx context.Context, project string, regions []string) (RegionAvailability, ScratchDiskSizes, error) {
	client, err := compute.NewMachineTypesRESTClient(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("creating machine types client: %w", err)
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
	scratchSizes := make(ScratchDiskSizes)

	it := client.AggregatedList(ctx, &computepb.AggregatedListMachineTypesRequest{
		Project: project,
	})
	for {
		pair, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("listing machine types: %w", err)
		}
		zone := strings.TrimPrefix(pair.Key, "zones/")
		region := zoneToRegion(zone)
		for _, mt := range pair.Value.GetMachineTypes() {
			name := mt.GetName()
			if regionSet[region] {
				availability[region][name] = true
			}
			// Scratch disk sizes are global (constant per machine type).
			if _, seen := scratchSizes[name]; !seen {
				partitions := 0
				if bls := mt.GetBundledLocalSsds(); bls != nil {
					partitions = int(bls.GetPartitionCount())
				}
				if gib := localssd.TotalGiB(name, partitions); gib > 0 {
					scratchSizes[name] = gib
				}
			}
		}
	}
	return availability, scratchSizes, nil
}

// fillLSSDSpotPrices derives gcp_web spot prices for -lssd machine types that are not
// listed on the spot pricing page. The spot page only lists the base machine type;
// the -lssd spot price is: base_spot + totalSSDGiB × spot_ssd_rate_per_GiB.
// Family-specific rates (c4, c4a, c4d) take priority over the "all" generic rate.
func fillLSSDSpotPrices(gcpWeb RegionPrices, ssdRates SSDSpotRates, scratchSizes ScratchDiskSizes) {
	for region, rates := range ssdRates {
		entry := gcpWeb[region]
		if entry.Spot == nil {
			continue
		}
		for machine, gib := range scratchSizes {
			if entry.Spot[machine] > 0 {
				continue // already have a direct web price
			}
			// Determine base name and SSD rate family from the machine type name.
			// Handles both foo-lssd and foo-lssd-metal (metal price = lssd base price).
			baseName, _, _ := strings.Cut(machine, "-metal") // strip -metal suffix if present
			baseName, isLSSD := strings.CutSuffix(baseName, "-lssd")
			if !isLSSD {
				continue // not an lssd variant; skip
			}

			baseSpot := entry.Spot[baseName]
			if baseSpot <= 0 {
				continue // base type has no web spot price either
			}

			// Map machine family prefix to the SSD rate table key.
			// e.g. "c4-standard-8" → family "c4" → key "c4".
			// C3/C3D -lssd machines use the generic "all" local SSD rate; C4/C4A/C4D
			// use family-specific included-LSSD rates. Other -lssd families without a
			// family-specific key have SSD bundled in the compute SKU, so skip them.
			family := machineFamily(baseName)
			rate, ok := rates[family]
			if (!ok || rate == 0) && (family == "c3" || family == "c3d") {
				rate, ok = rates["all"]
			}
			if !ok || rate == 0 {
				continue
			}

			entry.Spot[machine] = baseSpot + float64(gib)*rate
		}
		gcpWeb[region] = entry
	}
}

// machineFamily extracts the family prefix from a machine type name.
// e.g. "c3d-standard-8" → "c3d", "n1-standard-96" → "n1".
func machineFamily(name string) string {
	family, _, _ := strings.Cut(name, "-")
	return family
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

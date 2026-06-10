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
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GCP pricing pages embed all regional prices in raw HTML as an AF_initDataCallback
// JSON blob — no JavaScript execution needed.
//
// The nested data array contains region pricing table blocks. Each block has:
//
//	block[0] = header section (column names)
//	block[1] = data rows (one per machine type)
//	block[2] = region label string, e.g. "Iowa (us-central1)"
//
// Within each cell (header or data), the visible text lives at path [3][1][1].
// Hourly prices look like: "$0.097118 / 1 hour"

const gcpWebUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36"

// Index positions within a region pricing block and its cells.
// These are the only way to navigate the schema-less JSON — documented here
// so the magic numbers are defined once with explanations.
const (
	idxBlockHeaders    = 0 // block[0] = header section
	idxBlockDataRows   = 1 // block[1] = data rows
	idxBlockRegionName = 2 // block[2] = "City Name (region-code)"

	idxCellContentWrapper = 3 // cell[3]       = outer content wrapper
	idxCellInnerWrapper   = 1 // cell[3][1]    = inner wrapper
	idxCellText           = 1 // cell[3][1][1] = visible text string
)

const maxSearchDepth = 18

var gcpWebPages = []struct {
	url  string
	spot bool
}{
	{"https://cloud.google.com/products/compute/pricing/general-purpose", false},
	{"https://cloud.google.com/products/compute/pricing/compute-optimized", false},
	{"https://cloud.google.com/products/compute/pricing/memory-optimized", false},
	{"https://cloud.google.com/products/compute/pricing/storage-optimized", false},
	{"https://cloud.google.com/products/compute/pricing/accelerator-optimized", false},
	{"https://cloud.google.com/spot-vms/pricing", true},
}

var (
	gcpRegionBlockRe    = regexp.MustCompile(`.+\([\w]+-[\w\d-]+\)$`)
	gcpRegionCodeRe     = regexp.MustCompile(`\(([\w][\w\d-]+)\)`)
	gcpHourlyPriceRe    = regexp.MustCompile(`\$([\d.]+)\s*/\s*1 hour`)
	gcpGibibytePriceRe  = regexp.MustCompile(`\$([\d.]+)\s*/\s*1 gibibyte hour`)
	gcpHTMLTagRe        = regexp.MustCompile(`<[^>]+>`)
	platformQualifierRe = regexp.MustCompile(`(?i)\s+(?:\w+\s+)+platform\s+only$`)
)

// jsonNode wraps an untyped JSON value to provide named accessors for the known
// structure of GCP pricing page embedded data, hiding raw index arithmetic.
type jsonNode struct{ v any }

func (n jsonNode) at(i int) jsonNode {
	arr, ok := n.v.([]any)
	if !ok || i < 0 || i >= len(arr) {
		return jsonNode{}
	}
	return jsonNode{arr[i]}
}

func (n jsonNode) asArray() []any   { arr, _ := n.v.([]any); return arr }
func (n jsonNode) asString() string { s, _ := n.v.(string); return s }
func (n jsonNode) len() int         { return len(n.asArray()) }

func (n jsonNode) regionLabel() string     { return n.at(idxBlockRegionName).asString() }
func (n jsonNode) headerSection() jsonNode { return n.at(idxBlockHeaders) }
func (n jsonNode) dataRows() jsonNode      { return n.at(idxBlockDataRows) }

// cellText extracts visible text from a pricing table cell.
// Path: cell[3][1][1] — outer wrapper → inner wrapper → text string.
func (n jsonNode) cellText() string {
	return n.at(idxCellContentWrapper).at(idxCellInnerWrapper).at(idxCellText).asString()
}

// gcpWebFile is the cache format for GCP web prices + local SSD spot rates.
// It extends the basic priceFile format with SSD rates so both are cached atomically.
type gcpWebFile struct {
	SavedAt  *time.Time   `json:"saved_at,omitempty"`
	Prices   RegionPrices `json:"prices"`
	SSDRates SSDSpotRates `json:"ssd_rates,omitempty"`
}

func getGCPWebPrices(ctx context.Context, workDir string, noCache bool, cacheTTL time.Duration) (RegionPrices, SSDSpotRates, error) {
	path := filepath.Join(workDir, "gcpweb.json")
	name := "gcpweb.json:"

	if !noCache {
		if f, err := os.Open(path); err == nil {
			var cached gcpWebFile
			if json.NewDecoder(f).Decode(&cached) == nil && cached.SavedAt != nil {
				if age := time.Since(*cached.SavedAt); age < cacheTTL {
					f.Close()
					fmt.Printf("  %-14s using cache (%.1fh old)\n", name, age.Hours())
					return cached.Prices, cached.SSDRates, nil
				}
			}
			f.Close()
		}
	}

	prices, ssdRates, err := fetchGCPWebPrices(ctx)
	if err != nil {
		return nil, nil, err
	}

	now := time.Now().UTC()
	data, _ := json.Marshal(&gcpWebFile{SavedAt: &now, Prices: prices, SSDRates: ssdRates})
	_ = os.WriteFile(path, data, 0644)

	fmt.Printf("  %-14s %d regions\n", name, len(prices))
	return prices, ssdRates, nil
}

func fetchGCPWebPrices(ctx context.Context) (RegionPrices, SSDSpotRates, error) {
	fmt.Printf("  gcp web:   scraping %d pricing pages in parallel...\n", len(gcpWebPages))

	type pageResult struct {
		prices   RegionPrices
		ssdRates SSDSpotRates
		url      string
		err      error
	}

	results := make([]pageResult, len(gcpWebPages))
	var wg sync.WaitGroup
	for i, page := range gcpWebPages {
		wg.Add(1)
		go func(idx int, url string, isSpot bool) {
			defer wg.Done()
			prices, ssdRates, err := fetchGCPWebPage(ctx, url, isSpot)
			results[idx] = pageResult{prices: prices, ssdRates: ssdRates, url: url, err: err}
		}(i, page.url, page.spot)
	}
	wg.Wait()

	out := make(RegionPrices)
	var outSSD SSDSpotRates
	for _, res := range results {
		if res.err != nil {
			return nil, nil, fmt.Errorf("%s: %w", res.url, res.err)
		}
		for region, entry := range res.prices {
			outEntry := out[region]
			if outEntry.OnDemand == nil {
				outEntry.OnDemand = make(map[string]float64)
				outEntry.Spot = make(map[string]float64)
			}
			maps.Copy(outEntry.OnDemand, entry.OnDemand)
			maps.Copy(outEntry.Spot, entry.Spot)
			out[region] = outEntry
		}
		if res.ssdRates != nil {
			outSSD = res.ssdRates
		}
	}

	// Fill spot prices for -metal machine types from the base type.
	// The spot pricing page lists only the base name; metal variants are priced identically.
	for region := range out {
		entry := out[region]
		for name := range entry.OnDemand {
			if !strings.HasSuffix(name, "-metal") {
				continue
			}
			if entry.Spot[name] > 0 {
				continue
			}
			base := strings.TrimSuffix(name, "-metal")
			if baseSpot := entry.Spot[base]; baseSpot > 0 {
				entry.Spot[name] = baseSpot
			}
		}
		out[region] = entry
	}

	return out, outSSD, nil
}

// fetchGCPWebPage pipeline: fetch HTML → extract embedded JSON → discover region blocks → parse prices.
// For the spot VMs page, also discovers and returns local SSD spot rate blocks.
func fetchGCPWebPage(ctx context.Context, url string, isSpot bool) (RegionPrices, SSDSpotRates, error) {
	html, err := httpGet(ctx, url)
	if err != nil {
		return nil, nil, err
	}
	root, err := extractInitDataJSON(html)
	if err != nil {
		return nil, nil, fmt.Errorf("extracting embedded JSON: %w", err)
	}
	prices := parsePricingTables(walkForBlocks(root, 0), isSpot)
	var ssdRates SSDSpotRates
	if isSpot {
		ssdRates = parseSSDRateTables(walkForSSDBlocks(root, 0))
	}
	return prices, ssdRates, nil
}

// hasSSDRows returns true when any data row cell contains "gibibyte", identifying local SSD pricing blocks.
func hasSSDRows(rows []any) bool {
	for _, rowWrap := range rows[:min(5, len(rows))] {
		entry := jsonNode{rowWrap}.at(0)
		for col := 1; col < min(entry.len(), 5); col++ {
			if nodeContainsText(entry.at(col), "gibibyte") {
				return true
			}
		}
	}
	return false
}

// isSSDBlock returns true when a node looks like a regional local SSD pricing block:
// same region-label structure as machine type blocks, but with gibibyte-priced rows.
func isSSDBlock(n jsonNode) bool {
	if n.len() < 3 {
		return false
	}
	if !gcpRegionBlockRe.MatchString(n.regionLabel()) {
		return false
	}
	rows := n.dataRows().asArray()
	return len(rows) > 0 && hasSSDRows(rows)
}

func walkForSSDBlocks(n jsonNode, depth int) []jsonNode {
	if depth > maxSearchDepth {
		return nil
	}
	arr := n.asArray()
	if len(arr) == 0 {
		return nil
	}
	if isSSDBlock(n) {
		return []jsonNode{n}
	}
	var found []jsonNode
	for _, child := range arr {
		found = append(found, walkForSSDBlocks(jsonNode{child}, depth+1)...)
	}
	return found
}

// parseSSDRateTables extracts local SSD spot $/GiB-hour rates per region and VM family.
// The table rows have a VM family name ("All", "C4", "C4A", "C4D") and a gibibyte price.
// "all" covers any family not listed individually.
func parseSSDRateTables(blocks []jsonNode) SSDSpotRates {
	result := make(SSDSpotRates)
	for _, block := range blocks {
		region := extractRegionCode(block)
		if region == "" {
			continue
		}
		rates := make(map[string]float64)
		for _, rowWrap := range block.dataRows().asArray() {
			entry := jsonNode{rowWrap}.at(0)
			if entry.len() < 2 {
				continue
			}
			familyRaw := strings.TrimSpace(stripHTML(entry.at(0).cellText()))
			if familyRaw == "" {
				continue
			}
			family := strings.ToLower(familyRaw)
			for col := 1; col < entry.len(); col++ {
				match := gcpGibibytePriceRe.FindStringSubmatch(entry.at(col).cellText())
				if match == nil {
					continue
				}
				val, err := strconv.ParseFloat(match[1], 64)
				if err != nil || val == 0 {
					continue
				}
				rates[family] = val
				break
			}
		}
		if len(rates) > 0 {
			result[region] = rates
		}
	}
	return result
}

func httpGet(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", gcpWebUserAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	const maxBodyBytes = 50 << 20 // 50 MiB — pricing pages are <5 MiB in practice
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	return string(b), err
}

// extractInitDataJSON locates the AF_initDataCallback JSON blob in the page HTML
// and decodes the data array.
func extractInitDataJSON(html string) (jsonNode, error) {
	_, after, ok := strings.Cut(html, "AF_initDataCallback({")
	if !ok {
		return jsonNode{}, fmt.Errorf("AF_initDataCallback not found in page HTML")
	}
	_, after, ok = strings.Cut(after, ", data:")
	if !ok {
		return jsonNode{}, fmt.Errorf("data key not found inside AF_initDataCallback")
	}
	if len(after) == 0 || after[0] != '[' {
		got := ""
		if len(after) > 0 {
			got = string(after[0])
		}
		return jsonNode{}, fmt.Errorf("expected JSON array after data key, got %q", got)
	}
	var data any
	if err := json.NewDecoder(strings.NewReader(after)).Decode(&data); err != nil {
		return jsonNode{}, fmt.Errorf("decoding JSON: %w", err)
	}
	return jsonNode{data}, nil
}

func walkForBlocks(n jsonNode, depth int) []jsonNode {
	if depth > maxSearchDepth {
		return nil
	}
	arr := n.asArray()
	if len(arr) == 0 {
		return nil
	}
	if isRegionBlock(n) {
		return []jsonNode{n}
	}
	var found []jsonNode
	for _, child := range arr {
		found = append(found, walkForBlocks(jsonNode{child}, depth+1)...)
	}
	return found
}

// isRegionBlock returns true when a node has a "City (region-code)" label at
// index 2 and pricing data rows at index 1.
func isRegionBlock(n jsonNode) bool {
	if n.len() < 3 {
		return false
	}
	if !gcpRegionBlockRe.MatchString(n.regionLabel()) {
		return false
	}
	rows := n.dataRows().asArray()
	return len(rows) > 0 && hasPricingRows(rows)
}

func hasPricingRows(rows []any) bool {
	for _, rowWrap := range rows[:min(20, len(rows))] {
		entry := jsonNode{rowWrap}.at(0)
		for col := 2; col < min(entry.len(), 10); col++ {
			if nodeContainsText(entry.at(col), "/ 1 hour", "/ 1 month") {
				return true
			}
		}
	}
	return false
}

func nodeContainsText(n jsonNode, subs ...string) bool {
	if s := n.asString(); s != "" {
		for _, sub := range subs {
			if strings.Contains(s, sub) {
				return true
			}
		}
		return false
	}
	for _, child := range n.asArray() {
		if nodeContainsText(jsonNode{child}, subs...) {
			return true
		}
	}
	return false
}

// normalizeMachineTypeName strips platform qualifier suffixes from pricing page
// machine type names. For example, "n1-standard-96 Skylake Platform only" →
// "n1-standard-96". Returns the unchanged name if no qualifier is found.
//
// Some pricing tables use U+00A0 (non-breaking space) between the machine type
// and qualifier; normalize it to a plain space before applying the suffix regex.
func normalizeMachineTypeName(name string) string {
	name = strings.ReplaceAll(name, " ", " ")
	return platformQualifierRe.ReplaceAllString(name, "")
}

// isNonMachineTypeEntry returns true for reference entries that are not standard
// Compute Engine machine types and should be excluded from comparison:
//   - Sole-tenant node types (e.g. "c4-node-192-384") — not standard VMs
//   - Standalone GPU add-on prices (e.g. "NVIDIA T4") — per-GPU prices, not machine types
//   - Custom machine type per-unit prices (e.g. "Custom vCPUs") — per-unit prices
//
// Call normalizeMachineTypeName before this to strip platform qualifier suffixes
// (e.g. "Skylake Platform only") so they don't incorrectly trigger exclusion.
func isNonMachineTypeEntry(name string) bool {
	return strings.Contains(name, "-node-") ||
		strings.HasPrefix(name, "NVIDIA ") ||
		strings.HasPrefix(name, "Custom ")
}

func parsePricingTables(blocks []jsonNode, isSpot bool) RegionPrices {
	keywords := []string{"default", "price (usd)", "on-demand"}
	if isSpot {
		keywords = []string{"spot pricing", "spot"}
	}
	result := make(RegionPrices)
	for _, block := range blocks {
		region := extractRegionCode(block)
		if region == "" {
			continue
		}
		priceCol := findPriceColumn(block, keywords)
		if priceCol < 0 {
			continue
		}
		parsed := parseDataRows(block, priceCol, isSpot)
		entry := result[region]
		if entry.OnDemand == nil {
			entry.OnDemand = make(map[string]float64)
			entry.Spot = make(map[string]float64)
		}
		maps.Copy(entry.OnDemand, parsed.OnDemand)
		maps.Copy(entry.Spot, parsed.Spot)
		result[region] = entry
	}
	return result
}

func extractRegionCode(block jsonNode) string {
	match := gcpRegionCodeRe.FindStringSubmatch(block.regionLabel())
	if match == nil {
		return ""
	}
	return match[1]
}

// findPriceColumn returns the column index whose header contains one of the keywords.
// Header path: block[0][0][0] = slice of column header cells.
func findPriceColumn(block jsonNode, keywords []string) int {
	headerCols := block.headerSection().at(0).at(0).asArray()
	for i, col := range headerCols {
		label := strings.ToLower(stripHTML(jsonNode{col}.cellText()))
		for _, kw := range keywords {
			if strings.Contains(label, kw) {
				return i
			}
		}
	}
	return -1
}

func parseDataRows(block jsonNode, priceCol int, isSpot bool) RegionEntry {
	out := RegionEntry{
		OnDemand: make(map[string]float64),
		Spot:     make(map[string]float64),
	}
	for _, rowWrap := range block.dataRows().asArray() {
		entry := jsonNode{rowWrap}.at(0)
		if entry.len() <= priceCol {
			continue
		}
		nameRaw := entry.at(0).cellText()
		if strings.Contains(nameRaw, "<b>") {
			continue
		}
		name := normalizeMachineTypeName(stripHTML(nameRaw))
		if name == "" || isNonMachineTypeEntry(name) {
			continue
		}
		priceMatch := gcpHourlyPriceRe.FindStringSubmatch(entry.at(priceCol).cellText())
		if priceMatch == nil {
			continue
		}
		val, err := strconv.ParseFloat(priceMatch[1], 64)
		if err != nil || val == 0 {
			continue
		}
		if isSpot {
			out.Spot[name] = val
		} else {
			out.OnDemand[name] = val
		}
	}
	return out
}

func stripHTML(s string) string {
	return strings.TrimSpace(gcpHTMLTagRe.ReplaceAllString(s, ""))
}

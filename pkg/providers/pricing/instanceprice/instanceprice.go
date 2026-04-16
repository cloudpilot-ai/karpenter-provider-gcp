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

package instanceprice

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var cyclenerdURL = "https://gcloud-compute.com/machine-types-regions.csv"
var csvHTTPClient = &http.Client{Timeout: 30 * time.Second}

type Prices struct {
	OnDemand map[string]float64 `json:"on_demand,omitempty"`
	Spot     map[string]float64 `json:"spot,omitempty"`
}

type Client struct{}

func New(_ context.Context) (*Client, error) {
	return &Client{}, nil
}

func (c *Client) Close() {}

func (c *Client) FetchRegionPrices(ctx context.Context, region string) (Prices, error) {
	all, err := fetchAll(ctx)
	if err != nil {
		return Prices{}, err
	}
	return all[region], nil
}

func (c *Client) FetchAllPrices(ctx context.Context) (map[string]Prices, error) {
	return fetchAll(ctx)
}

func IsBlacklisted(_ string) bool {
	return false
}

func fetchAll(ctx context.Context) (map[string]Prices, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cyclenerdURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	resp, err := csvHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching Cyclenerd CSV: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cyclenerd CSV: HTTP %d", resp.StatusCode)
	}
	return parseCSV(resp.Body)
}

func parseCSV(r io.Reader) (map[string]Prices, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1

	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("reading CSV header: %w", err)
	}

	nameCol, regionCol, onDemandCol, spotCol, err := findColumns(header)
	if err != nil {
		return nil, err
	}

	return readRows(cr, nameCol, regionCol, onDemandCol, spotCol)
}

func findColumns(header []string) (nameCol, regionCol, onDemandCol, spotCol int, err error) {
	nameCol, regionCol, onDemandCol, spotCol = -1, -1, -1, -1
	for i, h := range header {
		switch strings.ToLower(h) {
		case "name":
			nameCol = i
		case "region":
			regionCol = i
		case "hour":
			onDemandCol = i
		case "hourspot":
			spotCol = i
		}
	}
	if nameCol < 0 || regionCol < 0 || onDemandCol < 0 || spotCol < 0 {
		return -1, -1, -1, -1, fmt.Errorf("CSV missing required columns (name/region/hour/hourSpot); got: %v", header)
	}
	return nameCol, regionCol, onDemandCol, spotCol, nil
}

func readRows(cr *csv.Reader, nameCol, regionCol, onDemandCol, spotCol int) (map[string]Prices, error) {
	result := make(map[string]Prices)
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading CSV row: %w", err)
		}
		if len(rec) <= max(nameCol, regionCol, onDemandCol, spotCol) {
			continue
		}
		onDemand, err := strconv.ParseFloat(rec[onDemandCol], 64)
		if err != nil || onDemand == 0 {
			continue
		}
		region := rec[regionCol]
		p := result[region]
		if p.OnDemand == nil {
			p.OnDemand = make(map[string]float64)
			p.Spot = make(map[string]float64)
		}
		p.OnDemand[rec[nameCol]] = onDemand
		if spot, err := strconv.ParseFloat(rec[spotCol], 64); err == nil && spot > 0 {
			p.Spot[rec[nameCol]] = spot
		}
		result[region] = p
	}
	return result, nil
}

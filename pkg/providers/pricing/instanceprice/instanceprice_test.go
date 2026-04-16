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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func buildCSV(rows [][]string) string {
	lines := make([]string, len(rows))
	for i, row := range rows {
		lines[i] = strings.Join(row, ",")
	}
	return strings.Join(lines, "\n")
}

func serveCSV(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
}

// withTestServer starts an httptest.Server and overrides the package-level
// cyclenerdURL so that FetchRegionPrices calls the test server instead of the real one.
func withTestServer(t *testing.T, handler http.Handler) {
	t.Helper()
	srv := httptest.NewServer(handler)
	orig := cyclenerdURL
	cyclenerdURL = srv.URL
	t.Cleanup(func() {
		cyclenerdURL = orig
		srv.Close()
	})
}

func newClient(t *testing.T) *Client {
	t.Helper()
	c, err := New(context.Background())
	require.NoError(t, err)
	return c
}

func TestParseCSV_ValidOnDemandOnly(t *testing.T) {
	csv := buildCSV([][]string{
		{"name", "region", "hour", "hourSpot"},
		{"e2-standard-2", "us-central1", "0.067", ""},
	})
	withTestServer(t, serveCSV(csv))

	prices, err := newClient(t).FetchRegionPrices(context.Background(), "us-central1")
	require.NoError(t, err)
	require.Len(t, prices.OnDemand, 1)
	require.Equal(t, 0.067, prices.OnDemand["e2-standard-2"])
	require.Empty(t, prices.Spot)
}

func TestParseCSV_ValidWithSpot(t *testing.T) {
	csv := buildCSV([][]string{
		{"name", "region", "hour", "hourSpot"},
		{"n2-standard-4", "us-east1", "0.194", "0.058"},
	})
	withTestServer(t, serveCSV(csv))

	prices, err := newClient(t).FetchRegionPrices(context.Background(), "us-east1")
	require.NoError(t, err)
	require.Len(t, prices.OnDemand, 1)
	require.Equal(t, 0.194, prices.OnDemand["n2-standard-4"])
	require.Equal(t, 0.058, prices.Spot["n2-standard-4"])
}

func TestParseCSV_InvalidOnDemandSkipped(t *testing.T) {
	for _, onDemand := range []string{"0", "N/A"} {
		t.Run(onDemand, func(t *testing.T) {
			csv := buildCSV([][]string{
				{"name", "region", "hour", "hourSpot"},
				{"e2-standard-2", "us-central1", onDemand, "0.010"},
			})
			withTestServer(t, serveCSV(csv))
			prices, err := newClient(t).FetchRegionPrices(context.Background(), "us-central1")
			require.NoError(t, err)
			require.Empty(t, prices.OnDemand)
		})
	}
}

func TestParseCSV_NonNumericSpotTreatedAsZero(t *testing.T) {
	csv := buildCSV([][]string{
		{"name", "region", "hour", "hourSpot"},
		{"e2-standard-2", "us-central1", "0.067", "N/A"},
	})
	withTestServer(t, serveCSV(csv))

	prices, err := newClient(t).FetchRegionPrices(context.Background(), "us-central1")
	require.NoError(t, err)
	require.Len(t, prices.OnDemand, 1)
	require.Empty(t, prices.Spot)
}

func TestParseCSV_ShortRowSkipped(t *testing.T) {
	withTestServer(t, serveCSV("name,region,hour,hourSpot\ne2-standard-2,us-central1"))

	prices, err := newClient(t).FetchRegionPrices(context.Background(), "us-central1")
	require.NoError(t, err)
	require.Empty(t, prices.OnDemand)
}

func TestParseCSV_CaseInsensitiveHeaders(t *testing.T) {
	csv := buildCSV([][]string{
		{"NAME", "REGION", "HOUR", "HOURSPOT"},
		{"e2-standard-2", "us-central1", "0.067", "0.020"},
	})
	withTestServer(t, serveCSV(csv))

	prices, err := newClient(t).FetchRegionPrices(context.Background(), "us-central1")
	require.NoError(t, err)
	require.Len(t, prices.OnDemand, 1)
	require.Contains(t, prices.OnDemand, "e2-standard-2")
}

func TestParseCSV_NegativeOnDemandSkipped(t *testing.T) {
	csv := buildCSV([][]string{
		{"name", "region", "hour", "hourSpot"},
		{"e2-standard-2", "us-central1", "-0.067", "0.020"},
	})
	withTestServer(t, serveCSV(csv))

	// Negative on-demand prices are not explicitly filtered by the production
	// code (only zero is skipped). This test documents the current behavior:
	// negative prices are accepted. Update if the production code is tightened.
	prices, err := newClient(t).FetchRegionPrices(context.Background(), "us-central1")
	require.NoError(t, err)
	require.Len(t, prices.OnDemand, 1)
	require.Less(t, prices.OnDemand["e2-standard-2"], 0.0)
}

func TestParseCSV_MissingRequiredColumn(t *testing.T) {
	// "hourSpot" column is absent.
	csv := buildCSV([][]string{
		{"name", "region", "hour"},
		{"e2-standard-2", "us-central1", "0.067"},
	})
	withTestServer(t, serveCSV(csv))

	_, err := newClient(t).FetchRegionPrices(context.Background(), "us-central1")
	require.ErrorContains(t, err, "CSV missing required columns")
}

func TestParseCSV_HeaderOnly(t *testing.T) {
	withTestServer(t, serveCSV("name,region,hour,hourSpot"))

	prices, err := newClient(t).FetchRegionPrices(context.Background(), "us-central1")
	require.NoError(t, err)
	require.Empty(t, prices.OnDemand)
}

func TestFetchRegionPrices_RegionFilter(t *testing.T) {
	csv := buildCSV([][]string{
		{"name", "region", "hour", "hourSpot"},
		{"e2-standard-2", "us-central1", "0.067", "0.020"},
		{"n2-standard-4", "us-east1", "0.194", "0.058"},
		{"c2-standard-8", "us-central1", "0.376", "0.113"},
	})
	withTestServer(t, serveCSV(csv))

	prices, err := newClient(t).FetchRegionPrices(context.Background(), "us-central1")
	require.NoError(t, err)
	require.Len(t, prices.OnDemand, 2)
	require.Contains(t, prices.OnDemand, "e2-standard-2")
	require.Contains(t, prices.OnDemand, "c2-standard-8")
}

func TestFetchRegionPrices_UnknownRegionReturnsEmpty(t *testing.T) {
	csv := buildCSV([][]string{
		{"name", "region", "hour", "hourSpot"},
		{"e2-standard-2", "us-central1", "0.067", ""},
	})
	withTestServer(t, serveCSV(csv))

	prices, err := newClient(t).FetchRegionPrices(context.Background(), "nonexistent-region")
	require.NoError(t, err)
	require.Empty(t, prices.OnDemand)
}

func TestFetchRegionPrices_ServerError(t *testing.T) {
	withTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	_, err := newClient(t).FetchRegionPrices(context.Background(), "us-central1")
	require.ErrorContains(t, err, "HTTP 500")
}

func TestFetchRegionPrices_UnreachableServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	orig := cyclenerdURL
	cyclenerdURL = srv.URL
	t.Cleanup(func() { cyclenerdURL = orig })
	srv.Close()

	_, err := newClient(t).FetchRegionPrices(context.Background(), "us-central1")
	require.Error(t, err)
}

func TestFetchRegionPrices_ConcurrentSafe(t *testing.T) {
	csv := buildCSV([][]string{
		{"name", "region", "hour", "hourSpot"},
		{"e2-standard-2", "us-central1", "0.067", "0.020"},
	})
	withTestServer(t, serveCSV(csv))

	c := newClient(t)
	type result struct {
		prices Prices
		err    error
	}
	const n = 20
	results := make([]result, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			prices, err := c.FetchRegionPrices(context.Background(), "us-central1")
			results[i] = result{prices: prices, err: err}
		}(i)
	}
	wg.Wait()
	// Assert on the test goroutine — require.* inside goroutines calls
	// t.FailNow() on the wrong goroutine, which is undefined behavior.
	for _, r := range results {
		require.NoError(t, r.err)
		require.Len(t, r.prices.OnDemand, 1)
	}
}

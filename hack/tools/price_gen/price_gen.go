/*
Copyright 2024 The CloudPilot AI Authors.

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
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
)

func main() {
	// Download the CSV file
	resp, err := http.Get("https://gcloud-compute.com/machine-types-regions.csv")
	if err != nil {
		fmt.Printf("Error downloading CSV: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	// Read the CSV data
	reader := csv.NewReader(resp.Body)
	reader.Comma = ','

	// Read header
	header, err := reader.Read()
	if err != nil {
		fmt.Printf("Error reading CSV header: %v\n", err)
		os.Exit(1)
	}

	// Find the required column indices
	priceColIndex := -1
	regionColIndex := -1
	machineTypeColIndex := -1

	for i, col := range header {
		switch col {
		case "hour":
			priceColIndex = i
		case "region":
			regionColIndex = i
		case "name":
			machineTypeColIndex = i
		}
	}

	if priceColIndex == -1 || regionColIndex == -1 || machineTypeColIndex == -1 {
		fmt.Println("Could not find required columns in CSV")
		os.Exit(1)
	}

	// Process the data
	allPrice := make(map[string]map[string]float64)

	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Printf("Error reading CSV record: %v\n", err)
			continue
		}

		// Get machine type, region and price
		machineType := record[machineTypeColIndex]
		region := record[regionColIndex]
		priceStr := record[priceColIndex]

		// Parse price
		price, err := strconv.ParseFloat(priceStr, 64)
		if err != nil {
			fmt.Printf("Error parsing price for %s in region %s: %v\n", machineType, region, err)
			continue
		}

		// Initialize region map if it doesn't exist
		if _, exists := allPrice[region]; !exists {
			allPrice[region] = make(map[string]float64)
		}

		// Store the price
		allPrice[region][machineType] = price
	}

	// Marshal to JSON
	jsonData, err := json.MarshalIndent(allPrice, "", "    ")
	if err != nil {
		fmt.Printf("Error marshaling JSON: %v\n", err)
		os.Exit(1)
	}

	// Write to file
	err = os.WriteFile("pkg/providers/pricing/initial-on-demand-prices.json", jsonData, 0644)
	if err != nil {
		fmt.Printf("Error writing JSON file: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Successfully generated pricing data")
}

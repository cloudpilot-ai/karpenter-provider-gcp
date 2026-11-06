/*
Copyright 2025 The CloudPilot AI Authors.

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
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/onsi/ginkgo/v2/types"
)

const maxNativeBytes = 16 << 20
const maxNativeSpecs = 20000

type specCounts struct {
	Executed int `json:"executed"`
	Passed   int `json:"passed"`
	Failed   int `json:"failed"`
	Skipped  int `json:"skipped"`
}

func readBoundedRegular(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > limit {
		return nil, errors.New("missing, unsafe or oversized report")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(data)) > limit {
		return nil, errors.New("oversized report")
	}
	return data, err
}

func readJSONReports(path string) ([]types.Report, specCounts, error) {
	data, err := readBoundedRegular(path, maxNativeBytes)
	if err != nil {
		return nil, specCounts{}, err
	}
	var reports []types.Report
	if err := json.Unmarshal(data, &reports); err != nil {
		return nil, specCounts{}, err
	}
	var counts specCounts
	total := 0
	if len(reports) == 0 || len(reports) > 64 {
		return nil, counts, errors.New("invalid suite count")
	}
	for _, report := range reports {
		total += len(report.SpecReports)
		if total > maxNativeSpecs {
			return nil, counts, errors.New("too many specs")
		}
		for _, spec := range report.SpecReports {
			if spec.LeafNodeType != types.NodeTypeIt {
				continue
			}
			switch {
			case spec.State == types.SpecStatePassed:
				counts.Passed++
				counts.Executed++
			case spec.State.Is(types.SpecStateFailureStates):
				counts.Failed++
				counts.Executed++
			case spec.State.Is(types.SpecStateSkipped | types.SpecStatePending):
				counts.Skipped++
			default:
				return nil, counts, errors.New("incomplete spec state")
			}
		}
	}
	if counts.Executed == 0 {
		return reports, counts, errors.New("no executed tests")
	}
	return reports, counts, nil
}

// Exclusive creation prevents PR-created files or symlinks redirecting evidence.
func writePrivateFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return errors.Join(err, f.Close())
}

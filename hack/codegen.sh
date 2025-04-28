#!/usr/bin/env bash

set -euo pipefail

echo "Updating pricing..."
go run hack/tools/price_gen/price_gen.go
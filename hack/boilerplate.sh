#!/bin/bash
set -eu -o pipefail

year=$(date +%Y)

for i in $(
  find ./cmd ./pkg ./hack -name "*.go"
); do
  if ! grep -q "Apache License, Version 2.0" $i; then
    { sed "s/YEAR/${year}/g" hack/boilerplate.go.txt; cat "$i"; } >"$i.new" && mv "$i.new" "$i"
  fi
done

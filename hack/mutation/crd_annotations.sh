#!/usr/bin/env bash

set -eu -o pipefail

# Inject the additionalAnnotations Helm template block into each CRD template.
# The block is inserted after the first "  annotations:" line found in each file.
for crd in charts/karpenter-crd/templates/*.yaml; do
    awk '
        {print}
        /  annotations:/ && !n {
            print "    {{- with .Values.additionalAnnotations }}"
            print "    {{- toYaml . | nindent 4 }}"
            print "    {{- end }}"
            n++
        }
    ' "$crd" > "$crd.new" && mv "$crd.new" "$crd"
done

#!/usr/bin/env bash
set -euo pipefail
# This whole job is credential-free and disposable after PR dependency processing.
if [[ ${1:-} != isolated ]]; then
  [[ $SOURCE_SHA =~ ^[a-f0-9]{40}$ && $PR_NUMBER =~ ^[1-9][0-9]*$ ]] || exit 1
  work="$RUNNER_TEMP/build-work"
  mkdir -m 700 "$work" "$RUNNER_TEMP/build-files"
  mkdir "$work/home" "$work/tmp" "$work/docker" "$work/helm"
  printf 'defaultPlatforms:\n  - linux/amd64\n' > "$work/ko.yaml"
  exec env -i PATH="$PATH" HOME="$work/home" TMPDIR="$work/tmp" \
    GOENV=off GOWORK=off GOFLAGS=-buildvcs=true CGO_ENABLED=0 GOTOOLCHAIN=local \
    GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_COUNT=0 GIT_TERMINAL_PROMPT=0 \
    KO_CONFIG_PATH="$work/ko.yaml" KO_DOCKER_REPO=e2e.invalid/karpenter DOCKER_CONFIG="$work/docker" \
    HELM_CONFIG_HOME="$work/helm" HELM_CACHE_HOME="$work/helm" HELM_DATA_HOME="$work/helm" \
    bash "$0" isolated "$work" "$RUNNER_TEMP/build-files" "$SOURCE_SHA" "$PR_NUMBER"
fi
work=$2 output=$3 sha=$4 pr=$5
mkdir "$work/source"
cd "$work/source"
git -c core.hooksPath=/dev/null init --quiet
git -c core.hooksPath=/dev/null -c protocol.file.allow=never fetch --no-tags --depth=1 \
  https://github.com/cloudpilot-ai/karpenter-provider-gcp.git "refs/pull/$pr/head"
[[ $(git rev-parse FETCH_HEAD) == "$sha" ]] || exit 1
git -c core.hooksPath=/dev/null checkout --detach "$sha"
[[ -z $(find . -type l -print -quit) ]] || exit 1
[[ -z $(git status --porcelain --untracked-files=all) ]] || exit 1
framework=$(go list -m -json github.com/onsi/ginkgo/v2)
jq -e '.Path == "github.com/onsi/ginkgo/v2" and .Replace == null and (.Version | test("^v[0-9]+\\.[0-9]+\\.[0-9]+$"))' <<< "$framework" > /dev/null
ko build --push=false --sbom=none --platform=linux/amd64 --tarball="$output/controller-image.tar" \
  --image-label="org.opencontainers.image.revision=$sha" \
  --ldflags="-X=sigs.k8s.io/karpenter/pkg/operator.Version=$sha" ./cmd/controller
for chart in karpenter karpenter-crd; do
  directory="charts/$chart"
  if helm dependency list "$directory" | awk 'NR>1 && NF {found=1} END {exit !found}'; then
    [[ -f $directory/Chart.lock && ! -L $directory/Chart.lock ]] || exit 1
  fi
  helm dependency build "$directory" --skip-refresh
  mkdir "$work/$chart"
  helm package "$directory" --destination "$work/$chart"
  packages=("$work/$chart/"*.tgz)
  [[ ${#packages[@]} == 1 && -f ${packages[0]} && ! -L ${packages[0]} ]] || exit 1
  if [[ $chart == karpenter ]]; then name=controller-chart.tgz; else name=crd-chart.tgz; fi
  mv -- "${packages[0]}" "$output/$name"
done
GOOS=linux GOARCH=amd64 go test -c -buildvcs=true -o "$output/e2e.test" ./test/suites
GOOS=linux GOARCH=amd64 go build -buildvcs=true -o "$output/ginkgo" github.com/onsi/ginkgo/v2/ginkgo
[[ $(git rev-parse HEAD) == "$sha" && -z $(git status --porcelain --untracked-files=all) ]] || exit 1

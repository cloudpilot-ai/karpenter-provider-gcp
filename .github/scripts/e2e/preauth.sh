#!/usr/bin/env bash
set -euo pipefail
# Run only after complete Octokit metadata and filesystem checks, before WIF.
[[ $SOURCE_SHA =~ ^[a-f0-9]{40}$ ]] || exit 1
root="$RUNNER_TEMP/prepared"
crane validate --tarball "$root/controller-image.tar/controller-image.tar"
for chart in controller-chart.tgz crd-chart.tgz; do helm show chart "$root/$chart/$chart" > /dev/null; done
for name in e2e.test ginkgo; do
  binary="$root/$name/$name"
  metadata=$(go version -m "$binary")
  grep -Eq '^[[:space:]]+build[[:space:]]+GOOS=linux$' <<< "$metadata"
  grep -Eq '^[[:space:]]+build[[:space:]]+GOARCH=amd64$' <<< "$metadata"
done
suite_metadata=$(go version -m "$root/e2e.test/e2e.test")
grep -Eq "^[[:space:]]+build[[:space:]]+vcs.revision=$SOURCE_SHA$" <<< "$suite_metadata"
grep -Eq '^[[:space:]]+build[[:space:]]+vcs.modified=false$' <<< "$suite_metadata"
# The dependency CLI has a mod row and no main-repository VCS stamp.
# Compare its module version with the suite dependency, without executing PR code.
test_version=$(go version -m "$root/e2e.test/e2e.test" | awk '$1 == "dep" && $2 == "github.com/onsi/ginkgo/v2" {print $3}')
cli_version=$(go version -m "$root/ginkgo/ginkgo" | awk '$1 == "mod" && $2 == "github.com/onsi/ginkgo/v2" {print $3}')
[[ $test_version =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ && $test_version == "$cli_version" ]] || exit 1
chmod 500 "$root/e2e.test/e2e.test" "$root/ginkgo/ginkgo"
e2e-runner validate-target

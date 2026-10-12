#!/usr/bin/env bash
# Exercise deployment argument wiring without cloud or Kubernetes access.
set -euo pipefail

REPO_ROOT="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

cat > "${TMP_DIR}/cli" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
case "$(basename "$0"):$*" in
  helm:*) printf '%s\n' "$*" >> "${HELM_LOG}" ;;
  gcloud:auth\ list*) echo maintainer@example.com ;;
  gcloud:auth\ configure-docker*) ;;
  ko:build*) echo example.invalid/karpenter@sha256:test ;;
  kubectl:config\ current-context) echo gke_example-project_asia-southeast1-b_tf-e2e-test-cluster ;;
  kubectl:get\ crd*) ;;
  kubectl:delete\ namespace\ karpenter-e2e-test*) ;;
  *) echo "Unexpected stub command: $0 $*" >&2; exit 1 ;;
esac
STUB
chmod +x "${TMP_DIR}/cli"
for cli in helm gcloud ko kubectl; do
  ln -s cli "${TMP_DIR}/${cli}"
done

export PATH="${TMP_DIR}:${PATH}"
export HELM_LOG="${TMP_DIR}/helm.log"
unset GOOGLE_APPLICATION_CREDENTIALS
export E2E_PROJECT_ID=example-project E2E_LOCATION=asia-southeast1-b
export E2E_PREFIX=tf-e2e-test E2E_REGION=asia-southeast1

check_deploy() {
  local version="$1" account="$2"
  : > "${HELM_LOG}"
  if ! make -C "${REPO_ROOT}" --no-print-directory e2e-deploy \
    RELEASE_VERSION="${version}" E2E_NODE_SERVICE_ACCOUNT="${account}" E2E_SA_PATH="" \
    > "${TMP_DIR}/deploy.log" 2>&1; then
    echo "Stubbed deployment failed (release=${version:-local})" >&2
    exit 1
  fi
  local controller_args
  controller_args="$(grep '^upgrade --install karpenter ' "${HELM_LOG}")"
  if [[ -n "${account}" ]]; then
    if [[ "${controller_args}" != *"--set-string controller.settings.defaultNodepoolServiceAccount=${account}"* ]]; then
      echo "Deployment did not select the requested node service account (release=${version:-local})" >&2
      exit 1
    fi
  elif [[ "${controller_args}" == *defaultNodepoolServiceAccount* ]]; then
    echo "Deployment overrode the legacy node service account default" >&2
    exit 1
  fi
}

check_deploy "" node@example-project.iam.gserviceaccount.com
check_deploy 0.7.0 node@example-project.iam.gserviceaccount.com
check_deploy "" ""
check_deploy 0.7.0 ""
echo "e2e-deploy argument tests passed"

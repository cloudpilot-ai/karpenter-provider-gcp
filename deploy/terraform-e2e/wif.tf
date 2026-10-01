locals {
  trusted_repository = "cloudpilot-ai/karpenter-provider-gcp"
  trusted_workflow   = "${local.trusted_repository}/.github/workflows/e2e-manual.yaml@refs/heads/main"
  trusted_subject    = "repo:${local.trusted_repository}:environment:e2e"
}

resource "google_iam_workload_identity_pool" "github" {
  count                     = var.enable_ci_wif ? 1 : 0
  project                   = var.project_id
  workload_identity_pool_id = "${var.prefix}-github"
  display_name              = "E2E GitHub Actions"
}

resource "google_iam_workload_identity_pool_provider" "github" {
  count                              = var.enable_ci_wif ? 1 : 0
  project                            = var.project_id
  workload_identity_pool_id          = google_iam_workload_identity_pool.github[0].workload_identity_pool_id
  workload_identity_pool_provider_id = "github-e2e"
  display_name                       = "Trusted E2E manual workflow"

  attribute_mapping = {
    "google.subject"          = "assertion.sub"
    "attribute.repository_id" = "assertion.repository_id"
  }
  attribute_condition = join(" && ", [
    "assertion.repository_id == '${var.github_repository_id}'",
    "assertion.repository_owner_id == '${var.github_owner_id}'",
    "assertion.repository == '${local.trusted_repository}'",
    "assertion.workflow_ref == '${local.trusted_workflow}'",
    "assertion.sub == '${local.trusted_subject}'",
    "assertion.ref == 'refs/heads/main'",
    "assertion.event_name == 'workflow_dispatch'",
  ])

  oidc {
    issuer_uri = "https://token.actions.githubusercontent.com"
  }
}

resource "google_service_account_iam_member" "runtime_wif" {
  count              = var.enable_ci_wif ? 1 : 0
  service_account_id = google_service_account.runtime.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "principalSet://iam.googleapis.com/${google_iam_workload_identity_pool.github[0].name}/attribute.repository_id/${var.github_repository_id}"
  depends_on         = [google_iam_workload_identity_pool_provider.github]
}

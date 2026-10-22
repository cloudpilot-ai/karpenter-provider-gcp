locals {
  ci_pool_id       = "${var.prefix}-ci"
  ci_runtime_role  = yamldecode(file("${path.module}/ci-runtime-role.yaml"))
  ci_provider_name = var.ci_enabled ? "projects/${data.google_project.ci[0].number}/locations/global/workloadIdentityPools/${local.ci_pool_id}/providers/github" : null
  ci_oidc_audience = var.ci_enabled ? "https://iam.googleapis.com/${local.ci_provider_name}" : null
}

data "google_project" "ci" {
  count      = var.ci_enabled ? 1 : 0
  project_id = var.project_id
}

resource "google_service_account" "ci_runtime" {
  count        = var.ci_enabled ? 1 : 0
  project      = var.project_id
  account_id   = "${var.prefix}-ci-sa"
  display_name = "Maintainer-dispatched e2e runtime"
}

resource "google_iam_workload_identity_pool" "ci" {
  count                     = var.ci_enabled ? 1 : 0
  project                   = var.project_id
  workload_identity_pool_id = local.ci_pool_id
  display_name              = "Upstream manual e2e"
}

resource "google_iam_workload_identity_pool_provider" "ci" {
  count                              = var.ci_enabled ? 1 : 0
  project                            = var.project_id
  workload_identity_pool_id          = google_iam_workload_identity_pool.ci[0].workload_identity_pool_id
  workload_identity_pool_provider_id = "github"
  attribute_mapping = {
    "google.subject"                = "assertion.sub"
    "attribute.repository_id"       = "assertion.repository_id"
    "attribute.repository_owner_id" = "assertion.repository_owner_id"
    "attribute.ref"                 = "assertion.ref"
    "attribute.workflow_ref"        = "assertion.workflow_ref"
    "attribute.event_name"          = "assertion.event_name"
    "attribute.environment"         = "assertion.environment"
  }
  attribute_condition = trimspace(file("${path.module}/ci-oidc-condition.cel"))
  oidc {
    issuer_uri        = "https://token.actions.githubusercontent.com"
    allowed_audiences = [local.ci_oidc_audience]
  }
}

resource "google_service_account_iam_member" "ci_federation" {
  count              = var.ci_enabled ? 1 : 0
  service_account_id = google_service_account.ci_runtime[0].name
  role               = "roles/iam.workloadIdentityUser"
  member             = "principalSet://iam.googleapis.com/${google_iam_workload_identity_pool.ci[0].name}/attribute.repository_id/863038341"
}

resource "google_project_iam_custom_role" "ci_runtime" {
  count       = var.ci_enabled ? 1 : 0
  project     = var.project_id
  role_id     = "${replace(var.prefix, "-", "_")}_ci_runtime"
  title       = local.ci_runtime_role.title
  description = local.ci_runtime_role.description
  stage       = local.ci_runtime_role.stage
  permissions = local.ci_runtime_role.includedPermissions
}

resource "google_project_iam_member" "ci_runtime" {
  count   = var.ci_enabled ? 1 : 0
  project = var.project_id
  role    = google_project_iam_custom_role.ci_runtime[0].id
  member  = "serviceAccount:${google_service_account.ci_runtime[0].email}"
}

resource "google_artifact_registry_repository_iam_member" "ci_push" {
  count      = var.ci_enabled ? 1 : 0
  project    = var.project_id
  location   = var.region
  repository = google_artifact_registry_repository.images.name
  role       = "roles/artifactregistry.writer"
  member     = "serviceAccount:${google_service_account.ci_runtime[0].email}"
}

resource "google_tags_tag_key_iam_member" "ci_read" {
  count   = var.ci_enabled ? 1 : 0
  tag_key = google_tags_tag_key.e2e.name
  role    = "roles/resourcemanager.tagViewer"
  member  = "serviceAccount:${google_service_account.ci_runtime[0].email}"
}

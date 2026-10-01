locals {
  controller_role = yamldecode(file("${path.module}/../iam/karpenter-controller-role.yaml"))
}

resource "google_service_account" "controller" {
  project      = var.project_id
  account_id   = local.ctrl_sa_id
  display_name = "E2E Karpenter controller"
}

resource "google_service_account" "node" {
  project      = var.project_id
  account_id   = local.node_sa_id
  display_name = "E2E GKE node"
}

resource "google_service_account" "runtime" {
  project      = var.project_id
  account_id   = local.runner_sa_id
  display_name = "E2E CI runtime"
}

resource "google_project_iam_custom_role" "controller" {
  project     = var.project_id
  role_id     = "${replace(var.prefix, "-", "_")}_controller"
  title       = local.controller_role.title
  description = local.controller_role.description
  permissions = local.controller_role.includedPermissions
}

resource "google_project_iam_member" "controller" {
  project = var.project_id
  role    = google_project_iam_custom_role.controller.id
  member  = google_service_account.controller.member
}

resource "google_service_account_iam_member" "controller_workload_identity" {
  service_account_id = google_service_account.controller.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[karpenter-system/karpenter]"
}

resource "google_service_account_iam_member" "controller_node_act_as" {
  service_account_id = google_service_account.node.name
  role               = "roles/iam.serviceAccountUser"
  member             = google_service_account.controller.member
}

resource "google_project_iam_member" "node" {
  project = var.project_id
  role    = "roles/container.nodeServiceAccount"
  member  = google_service_account.node.member
}

resource "google_artifact_registry_repository_iam_member" "node_pull" {
  project    = var.project_id
  location   = var.region
  repository = google_artifact_registry_repository.images.name
  role       = "roles/artifactregistry.reader"
  member     = google_service_account.node.member
}

resource "google_project_iam_custom_role" "runtime_reads" {
  project     = var.project_id
  role_id     = "${replace(var.prefix, "-", "_")}_ci_reads"
  title       = "E2E CI target reads"
  description = "Read-only cloud operations used by e2e runner and test suites"
  permissions = [
    "compute.disks.get",
    "compute.instances.get",
    "compute.regions.get",
    "compute.zones.get",
    "container.clusters.get",
    "container.nodePools.list",
    "container.serverConfig.get",
  ]
}

resource "google_project_iam_member" "runtime_reads" {
  project = var.project_id
  role    = google_project_iam_custom_role.runtime_reads.id
  member  = google_service_account.runtime.member
}

resource "google_artifact_registry_repository_iam_member" "runtime_push" {
  project    = var.project_id
  location   = var.region
  repository = google_artifact_registry_repository.images.name
  role       = "roles/artifactregistry.writer"
  member     = google_service_account.runtime.member
}

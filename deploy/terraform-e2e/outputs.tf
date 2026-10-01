output "project_id" {
  value = var.project_id
}

output "region" {
  value = var.region
}

output "location" {
  value = var.location
}

output "prefix" {
  value = var.prefix
}

output "cluster_name" {
  value = google_container_cluster.e2e.name
}

output "image_repository" {
  value = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.images.repository_id}/karpenter"
}

output "controller_service_account" {
  value = google_service_account.controller.email
}

output "node_service_account" {
  value = google_service_account.node.email
}

output "runtime_service_account" {
  value = google_service_account.runtime.email
}

output "wif_provider" {
  value = var.enable_ci_wif ? google_iam_workload_identity_pool_provider.github[0].name : null
}

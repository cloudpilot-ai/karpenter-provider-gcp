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
  value = module.cluster.cluster_name
}

output "image_repository" {
  value = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.images.repository_id}/karpenter"
}

output "controller_service_account" {
  value = module.cluster.karpenter_controller_sa_email
}

output "node_service_account" {
  value = module.cluster.karpenter_node_sa_email
}

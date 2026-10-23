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

output "pods_range_name" {
  value = "${var.prefix}-pods"
}

output "additional_pod_ranges" {
  value = var.additional_pod_ranges
}

output "resource_manager_tag_key" {
  value = {
    id         = google_tags_tag_key.e2e.id
    name       = google_tags_tag_key.e2e.name
    short_name = google_tags_tag_key.e2e.short_name
  }
}

output "ci_workload_identity_provider" {
  value       = local.ci_provider_name
  description = "google-github-actions/auth workload_identity_provider; null when CI is off."
}

output "ci_oidc_audience" {
  value       = local.ci_oidc_audience
  description = "Explicit auth action audience; provider accepts only this exact audience."
}

output "ci_runtime_service_account" {
  value = one(google_service_account.ci_runtime[*].email)
}

output "ci_environment_variables" {
  description = "Reviewed non-secret e2e-runtime target configuration, not authorization to activate runs. Null when CI is off."
  value = var.ci_enabled ? {
    E2E_PROJECT_ID                 = var.project_id
    E2E_REGION                     = var.region
    E2E_LOCATION                   = var.location
    E2E_PREFIX                     = var.prefix
    E2E_CLUSTER_NAME               = module.cluster.cluster_name
    E2E_IMAGE_REPOSITORY           = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.images.repository_id}/karpenter"
    E2E_CONTROLLER_SERVICE_ACCOUNT = module.cluster.karpenter_controller_sa_email
    E2E_NODE_SERVICE_ACCOUNT       = module.cluster.karpenter_node_sa_email
    E2E_PODS_RANGE_NAME            = "${var.prefix}-pods"
    E2E_SMALL_PODS_RANGE_NAME      = try(var.ci_pod_range_names.small, null)
    E2E_LARGE_PODS_RANGE_NAME      = try(var.ci_pod_range_names.large, null)
    E2E_RESOURCE_MANAGER_TAG_KEY   = google_tags_tag_key.e2e.short_name
  } : null
}

resource "google_container_node_pool" "system" {
  project    = var.project_id
  name       = "${var.prefix}-system"
  location   = var.location
  cluster    = module.cluster.cluster_name
  node_count = 1

  node_config {
    machine_type    = var.system_machine_type
    disk_size_gb    = 30
    service_account = module.cluster.karpenter_node_sa_email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]

    workload_metadata_config {
      mode = "GKE_METADATA"
    }
  }

  depends_on = [google_artifact_registry_repository_iam_member.node_pull, google_compute_router_nat.e2e]

  lifecycle {
    prevent_destroy = true
  }
}

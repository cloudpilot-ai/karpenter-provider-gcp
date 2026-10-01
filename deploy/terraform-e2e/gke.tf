resource "google_container_cluster" "e2e" {
  project  = var.project_id
  name     = local.cluster_name
  location = var.location

  network    = google_compute_network.e2e.id
  subnetwork = google_compute_subnetwork.e2e.id

  remove_default_node_pool = true
  initial_node_count       = 1
  deletion_protection      = true
  resource_labels          = { "e2e-owner" = "terraform" }

  release_channel {
    channel = "REGULAR"
  }

  workload_identity_config {
    workload_pool = "${var.project_id}.svc.id.goog"
  }

  private_cluster_config {
    enable_private_nodes    = true
    enable_private_endpoint = false
    master_ipv4_cidr_block  = var.control_plane_cidr
  }

  ip_allocation_policy {
    cluster_secondary_range_name  = "${var.prefix}-pods"
    services_secondary_range_name = "${var.prefix}-services"
  }

  logging_config {
    enable_components = []
  }
  monitoring_config {
    enable_components = []
    managed_prometheus {
      enabled = false
    }
  }
}

resource "google_container_node_pool" "system" {
  project    = var.project_id
  name       = "${var.prefix}-system"
  location   = var.location
  cluster    = google_container_cluster.e2e.name
  node_count = 1

  node_config {
    machine_type    = var.system_machine_type
    disk_size_gb    = 30
    service_account = google_service_account.node.email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]
  }

  depends_on = [google_project_iam_member.node, google_artifact_registry_repository_iam_member.node_pull, google_compute_router_nat.e2e]

  lifecycle {
    prevent_destroy = true
  }
}

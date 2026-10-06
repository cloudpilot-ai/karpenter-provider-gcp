resource "google_compute_router" "e2e" {
  project = var.project_id
  name    = "${var.prefix}-router"
  region  = var.region
  network = module.cluster.network_id
}

resource "google_compute_router_nat" "e2e" {
  project                            = var.project_id
  name                               = "${var.prefix}-nat"
  router                             = google_compute_router.e2e.name
  region                             = var.region
  nat_ip_allocate_option             = "AUTO_ONLY"
  source_subnetwork_ip_ranges_to_nat = "ALL_SUBNETWORKS_ALL_IP_RANGES"
  log_config {
    enable = true
    filter = "ERRORS_ONLY"
  }
}

resource "google_artifact_registry_repository" "images" {
  project       = var.project_id
  location      = var.region
  repository_id = "${var.prefix}-images"
  format        = "DOCKER"

  cleanup_policies {
    id     = "delete-after-seven-days"
    action = "DELETE"

    condition {
      tag_state  = "ANY"
      older_than = "604800s"
    }
  }
}

resource "google_artifact_registry_repository_iam_member" "node_pull" {
  project    = var.project_id
  location   = var.region
  repository = google_artifact_registry_repository.images.name
  role       = "roles/artifactregistry.reader"
  member     = "serviceAccount:${module.cluster.karpenter_node_sa_email}"
}

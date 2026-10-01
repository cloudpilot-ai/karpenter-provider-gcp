locals {
  cluster_name = "${var.prefix}-cluster"
  node_sa_id   = "${var.prefix}-node"
  ctrl_sa_id   = "${var.prefix}-karpenter"
  runner_sa_id = "${var.prefix}-ci"
}

resource "google_compute_network" "e2e" {
  project                 = var.project_id
  name                    = "${var.prefix}-vpc"
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "e2e" {
  project                  = var.project_id
  name                     = "${var.prefix}-subnet"
  region                   = var.region
  network                  = google_compute_network.e2e.id
  ip_cidr_range            = var.primary_cidr
  private_ip_google_access = true

  secondary_ip_range {
    range_name    = "${var.prefix}-pods"
    ip_cidr_range = var.pods_cidr
  }
  secondary_ip_range {
    range_name    = "${var.prefix}-services"
    ip_cidr_range = var.services_cidr
  }
}

resource "google_compute_router" "e2e" {
  project = var.project_id
  name    = "${var.prefix}-router"
  region  = var.region
  network = google_compute_network.e2e.id
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
}

terraform {
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "6.28.0"
    }
  }
}

resource "google_compute_network" "default" {
  project = var.project_id
  name    = coalesce(var.network_name, var.common_name)

  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "default" {
  project = var.project_id
  name    = coalesce(var.subnetwork_name, var.common_name)

  ip_cidr_range            = var.primary_cidr
  region                   = var.google_region
  private_ip_google_access = var.private_nodes

  network = google_compute_network.default.id
  secondary_ip_range {
    range_name    = var.services_range_name
    ip_cidr_range = var.services_cidr
  }

  secondary_ip_range {
    range_name    = var.pods_range_name
    ip_cidr_range = var.pods_cidr
  }

  dynamic "secondary_ip_range" {
    for_each = var.additional_pod_ranges
    content {
      range_name    = secondary_ip_range.key
      ip_cidr_range = secondary_ip_range.value
    }
  }
}

resource "google_container_cluster" "default" {
  project = var.project_id
  name    = coalesce(var.cluster_name, var.common_name)

  location           = coalesce(var.cluster_location, var.google_region)
  min_master_version = var.kubernetes_version

  network    = google_compute_network.default.id
  subnetwork = google_compute_subnetwork.default.id

  ip_allocation_policy {
    services_secondary_range_name = var.services_range_name
    cluster_secondary_range_name  = var.pods_range_name

    dynamic "additional_pod_ranges_config" {
      for_each = length(var.additional_pod_ranges) > 0 ? [1] : []
      content {
        pod_range_names = keys(var.additional_pod_ranges)
      }
    }
  }

  deletion_protection = var.deletion_protection

  remove_default_node_pool = true
  initial_node_count       = 1

  workload_identity_config {
    workload_pool = "${var.project_id}.svc.id.goog"
  }

  dynamic "private_cluster_config" {
    for_each = var.private_nodes ? [1] : []
    content {
      enable_private_nodes    = true
      enable_private_endpoint = false
      master_ipv4_cidr_block  = var.control_plane_cidr
    }
  }

  lifecycle {
    precondition {
      condition     = !contains(keys(var.additional_pod_ranges), var.pods_range_name) && !contains(keys(var.additional_pod_ranges), var.services_range_name)
      error_message = "Additional pod range names must differ from the default pod and services range names."
    }

    precondition {
      condition     = !var.private_nodes || var.control_plane_cidr != null
      error_message = "control_plane_cidr is required for private nodes."
    }
  }
}

variable "common_name" {
  type = string
  default = "karpenter-provider-gcp"
}

variable "google_region" {
  type = string
  default = "us-central1"
}

variable "project_id" {
  type = string
  default = "karpenter-provider-gcp"
}

terraform {
  required_providers {
    google = {
      source = "hashicorp/google"
      version = "6.28.0"
    }
  }
}

provider "google" {
  project = var.project_id
}

resource "google_compute_network" "default" {
  name = var.common_name

  auto_create_subnetworks  = false
  enable_ula_internal_ipv6 = false
}

resource "google_compute_subnetwork" "default" {
  name = var.common_name

  ip_cidr_range = "10.0.0.0/24"
  region        = var.google_region

  network = google_compute_network.default.id
  secondary_ip_range {
    range_name    = "services-range"
    ip_cidr_range = "10.10.0.0/22"
  }

  secondary_ip_range {
    range_name    = "pod-ranges"
    ip_cidr_range = "10.100.0.0/20"
  }
}

resource "google_container_cluster" "default" {
  name = var.common_name

  location                 = var.google_region

  network    = google_compute_network.default.id
  subnetwork = google_compute_subnetwork.default.id

  monitoring_config {
    enable_components = []
    managed_prometheus {
        enabled = false
    }
  }

  logging_config {
    enable_components = []
  }

  ip_allocation_policy {
    services_secondary_range_name = google_compute_subnetwork.default.secondary_ip_range[0].range_name
    cluster_secondary_range_name  = google_compute_subnetwork.default.secondary_ip_range[1].range_name
  }

  deletion_protection = false

  remove_default_node_pool = true
  initial_node_count       = 1
}

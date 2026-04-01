locals {
  name_prefix = substr(replace(lower(var.prefix), "_", "-"), 0, 20)

  cluster_name         = "${local.name_prefix}-cluster"
  network_name         = "${local.name_prefix}-vpc"
  subnet_name          = "${local.name_prefix}-subnet"
  pods_range_name      = "${local.name_prefix}-pods"
  services_range_name  = "${local.name_prefix}-svc"
  karpenter_sa_id      = "${local.name_prefix}-karpenter"
  node_sa_id           = "${local.name_prefix}-nodes"
  system_nodepool_name = "${local.name_prefix}-default"
  zones = [
    "${var.region}-a",
    "${var.region}-b",
    "${var.region}-c",
  ]
}

resource "google_project_service" "compute" {
  project            = var.project_id
  service            = "compute.googleapis.com"
  disable_on_destroy = false
}

resource "google_project_service" "container" {
  project            = var.project_id
  service            = "container.googleapis.com"
  disable_on_destroy = false
}

resource "google_compute_network" "e2e" {
  name                    = local.network_name
  auto_create_subnetworks = false

  depends_on = [
    google_project_service.compute,
  ]
}

resource "google_compute_subnetwork" "e2e" {
  name          = local.subnet_name
  region        = var.region
  network       = google_compute_network.e2e.id
  ip_cidr_range = "10.10.0.0/20"

  secondary_ip_range {
    range_name    = local.pods_range_name
    ip_cidr_range = "10.20.0.0/16"
  }

  secondary_ip_range {
    range_name    = local.services_range_name
    ip_cidr_range = "10.30.0.0/20"
  }
}

resource "google_service_account" "karpenter" {
  account_id   = local.karpenter_sa_id
  display_name = "${var.prefix} Karpenter controller"
}

resource "google_service_account" "nodes" {
  account_id   = local.node_sa_id
  display_name = "${var.prefix} GKE nodes"
}

resource "google_project_iam_member" "karpenter_compute_admin" {
  project = var.project_id
  role    = "roles/compute.admin"
  member  = "serviceAccount:${google_service_account.karpenter.email}"
}

resource "google_project_iam_member" "karpenter_container_admin" {
  project = var.project_id
  role    = "roles/container.admin"
  member  = "serviceAccount:${google_service_account.karpenter.email}"
}

resource "google_project_iam_member" "karpenter_sa_user" {
  project = var.project_id
  role    = "roles/iam.serviceAccountUser"
  member  = "serviceAccount:${google_service_account.karpenter.email}"
}

resource "google_project_iam_member" "nodes_default_permissions" {
  project = var.project_id
  role    = "roles/container.defaultNodeServiceAccount"
  member  = "serviceAccount:${google_service_account.nodes.email}"
}

resource "google_service_account_iam_member" "karpenter_workload_identity" {
  service_account_id = google_service_account.karpenter.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[karpenter-system/karpenter]"
}

resource "google_container_cluster" "e2e" {
  provider = google-beta

  name                = local.cluster_name
  location            = var.region
  network             = google_compute_network.e2e.id
  subnetwork          = google_compute_subnetwork.e2e.name
  deletion_protection = false

  networking_mode          = "VPC_NATIVE"
  remove_default_node_pool = true
  initial_node_count       = 1

  workload_identity_config {
    workload_pool = "${var.project_id}.svc.id.goog"
  }

  ip_allocation_policy {
    cluster_secondary_range_name  = local.pods_range_name
    services_secondary_range_name = local.services_range_name
  }

  release_channel {
    channel = "REGULAR"
  }

  depends_on = [
    google_project_service.compute,
    google_project_service.container,
  ]
}

resource "google_container_node_pool" "default" {
  name           = local.system_nodepool_name
  cluster        = google_container_cluster.e2e.name
  location       = google_container_cluster.e2e.location
  node_locations = local.zones

  autoscaling {
    total_min_node_count = 1
    total_max_node_count = 3
  }

  management {
    auto_repair  = true
    auto_upgrade = true
  }

  node_config {
    machine_type    = "e2-medium"
    service_account = google_service_account.nodes.email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]

    labels = {
      pool = local.system_nodepool_name
    }
  }

  depends_on = [
    google_project_iam_member.nodes_default_permissions,
    google_container_cluster.e2e,
  ]
}

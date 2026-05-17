variable "common_name" {
  type    = string
  default = "karpenter-provider-gcp"
  validation {
    condition     = length("${var.common_name}-ctrl") <= 30
    error_message = "common_name + '-ctrl' must be ≤ 30 characters for a valid GCP service account ID."
  }
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

# --- IAM: Karpenter controller service account and minimal role ---

variable "kubernetes_namespace" {
  type        = string
  default     = "karpenter-system"
  description = "Kubernetes namespace where the Karpenter controller runs."
}

variable "kubernetes_service_account" {
  type        = string
  default     = "karpenter"
  description = "Kubernetes service account name for the Karpenter controller."
}

variable "node_service_account_email" {
  type        = string
  description = "Email of the GCP SA that Karpenter attaches to provisioned nodes (iam.serviceAccountUser is bound here). Leave empty to skip."
  default     = ""
}

locals {
  iam_role = yamldecode(file("${path.module}/../iam/karpenter-controller-role.yaml"))
}

resource "google_service_account" "karpenter_controller" {
  account_id   = "${var.common_name}-ctrl"
  display_name = "Karpenter controller"
  project      = var.project_id
}

resource "google_project_iam_custom_role" "karpenter_controller" {
  role_id     = "karpenter_controller"
  title       = local.iam_role.title
  description = local.iam_role.description
  permissions = local.iam_role.includedPermissions
  project     = var.project_id
}

resource "google_project_iam_member" "karpenter_controller" {
  project = var.project_id
  role    = google_project_iam_custom_role.karpenter_controller.id
  member  = "serviceAccount:${google_service_account.karpenter_controller.email}"
}

resource "google_service_account_iam_member" "workload_identity" {
  service_account_id = google_service_account.karpenter_controller.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[${var.kubernetes_namespace}/${var.kubernetes_service_account}]"
}

resource "google_service_account_iam_member" "node_sa_actAs" {
  count              = var.node_service_account_email != "" ? 1 : 0
  service_account_id = "projects/${var.project_id}/serviceAccounts/${var.node_service_account_email}"
  role               = "roles/iam.serviceAccountUser"
  member             = "serviceAccount:${google_service_account.karpenter_controller.email}"
}

output "karpenter_controller_sa_email" {
  value = google_service_account.karpenter_controller.email
}

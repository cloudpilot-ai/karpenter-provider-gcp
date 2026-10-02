locals {
  cluster_name = "${var.prefix}-cluster"
}

module "cluster" {
  source = "../terraform"

  project_id                    = var.project_id
  google_region                 = var.region
  cluster_location              = var.location
  common_name                   = var.prefix
  network_name                  = "${var.prefix}-vpc"
  subnetwork_name               = "${var.prefix}-subnet"
  cluster_name                  = local.cluster_name
  controller_service_account_id = "${var.prefix}-karpenter"
  primary_cidr                  = var.primary_cidr
  pods_cidr                     = var.pods_cidr
  services_cidr                 = var.services_cidr
  pods_range_name               = "${var.prefix}-pods"
  services_range_name           = "${var.prefix}-services"
  private_nodes                 = true
  control_plane_cidr            = var.control_plane_cidr
  deletion_protection           = true
}

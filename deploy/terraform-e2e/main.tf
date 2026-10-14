locals {
  cluster_name = "${var.prefix}-cluster"
}

module "cluster" {
  source = "../terraform"

  project_id                    = var.project_id
  google_region                 = var.region
  cluster_location              = var.location
  release_channel               = var.release_channel
  node_locations                = var.node_locations
  system_node_pool_name         = "default-pool"
  system_node_locations         = ["asia-southeast1-b"]
  system_machine_type           = var.system_machine_type
  system_node_count             = 1
  system_disk_size_gb           = 30
  additional_pod_ranges         = var.additional_pod_ranges
  common_name                   = var.prefix
  network_name                  = "${var.prefix}-vpc"
  subnetwork_name               = "${var.prefix}-subnet"
  cluster_name                  = local.cluster_name
  controller_service_account_id = "${var.prefix}-karpenter"
  controller_role_id            = "karpenter_controller"
  primary_cidr                  = var.primary_cidr
  pods_cidr                     = var.pods_cidr
  services_cidr                 = var.services_cidr
  pods_range_name               = "${var.prefix}-pods"
  services_range_name           = "${var.prefix}-services"
  private_nodes                 = true
  control_plane_cidr            = var.control_plane_cidr
  deletion_protection           = false
  datapath_provider             = "ADVANCED_DATAPATH"
  cloud_observability_enabled   = false
}

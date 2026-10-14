resource "google_container_node_pool" "system" {
  project        = var.project_id
  name           = coalesce(var.system_node_pool_name, "${var.common_name}-system")
  location       = google_container_cluster.default.location
  cluster        = google_container_cluster.default.name
  node_count     = var.system_node_count
  node_locations = var.system_node_locations

  # GKE assigns a pod range at pool creation. With additional pod ranges attached
  # to the cluster, the pool can otherwise occupy one of them and pin it, which
  # blocks resizing or removing that range later.
  dynamic "network_config" {
    for_each = var.system_pod_range == null ? [] : [1]
    content {
      pod_range = var.system_pod_range
    }
  }

  node_config {
    machine_type    = var.system_machine_type
    disk_size_gb    = var.system_disk_size_gb
    service_account = local.node_sa_email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]

    workload_metadata_config {
      mode = "GKE_METADATA"
    }
  }

  depends_on = [google_project_iam_member.karpenter_node, google_project_iam_member.karpenter_node_ar_reader]

}

mock_provider "google" {}

run "defaults" {
  command = plan

  assert {
    condition     = google_container_cluster.default.min_master_version == null
    error_message = "Standalone installs must retain GKE's default version selection."
  }

  assert {
    condition     = length(google_compute_subnetwork.default.secondary_ip_range) == 2 && length(google_container_cluster.default.ip_allocation_policy[0].additional_pod_ranges_config) == 0
    error_message = "No additional pod ranges should be created or attached by default."
  }
}

run "version_136" {
  command = plan

  variables {
    kubernetes_version = "1.36"
  }

  assert {
    condition     = google_container_cluster.default.min_master_version == "1.36"
    error_message = "The version input must also support 1.36."
  }
}

run "invalid_cidr" {
  command = plan

  variables {
    additional_pod_ranges = { extra = "not-a-cidr" }
  }

  expect_failures = [var.additional_pod_ranges]
}

run "reserved_services_name" {
  command = plan

  variables {
    additional_pod_ranges = { services-range = "10.104.0.0/24" }
  }

  expect_failures = [google_container_cluster.default]
}

run "reserved_pods_name" {
  command = plan

  variables {
    additional_pod_ranges = { pod-ranges = "10.104.0.0/24" }
  }

  expect_failures = [google_container_cluster.default]
}

run "additional_ranges" {
  command = plan

  variables {
    additional_pod_ranges = {
      pods-small = "10.104.0.0/24"
      pods-large = "10.105.0.0/20"
    }
  }

  assert {
    condition = alltrue([
      for name, cidr in var.additional_pod_ranges : contains([
        for range in google_compute_subnetwork.default.secondary_ip_range : "${range.range_name}=${range.ip_cidr_range}"
      ], "${name}=${cidr}")
    ])
    error_message = "Every additional range must exist on the subnet with its requested CIDR."
  }

  assert {
    condition     = toset(google_container_cluster.default.ip_allocation_policy[0].additional_pod_ranges_config[0].pod_range_names) == toset(keys(var.additional_pod_ranges))
    error_message = "GKE must adopt every additional subnet pod range."
  }

  assert {
    condition     = google_container_cluster.default.ip_allocation_policy[0].cluster_secondary_range_name == var.pods_range_name && google_container_cluster.default.ip_allocation_policy[0].services_secondary_range_name == var.services_range_name
    error_message = "Additional ranges must not change the default pod or services range."
  }
}

run "version_135" {
  command = plan

  variables {
    kubernetes_version = "1.35"
  }

  assert {
    condition     = google_container_cluster.default.min_master_version == "1.35"
    error_message = "The requested Kubernetes version must reach the GKE cluster."
  }
}

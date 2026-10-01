mock_provider "google" {}

run "persistent_target" {
  command = plan

  variables {
    control_plane_cidr   = "10.12.0.0/28"
    project_id           = "sponsored-example-123"
    region               = "asia-southeast1"
    location             = "asia-southeast1-b"
    prefix               = "tf-e2e-sgp"
    system_machine_type  = "n2-standard-2"
    primary_cidr         = "10.0.0.0/20"
    pods_cidr            = "10.4.0.0/14"
    services_cidr        = "10.8.0.0/20"
    github_repository_id = "863038341"
    github_owner_id      = "166281999"
  }

  assert {
    condition     = google_container_cluster.e2e.deletion_protection && google_container_node_pool.system.node_count == 1
    error_message = "The persistent cluster must be protected and have one system node."
  }

  assert {
    condition     = google_project_iam_custom_role.controller.permissions == toset(local.controller_role.includedPermissions)
    error_message = "Controller permissions must be read from deploy/iam/karpenter-controller-role.yaml."
  }

  assert {
    condition     = length(google_iam_workload_identity_pool_provider.github) == 0
    error_message = "WIF must remain disabled until the workflow contract is approved."
  }

  assert {
    condition     = output.region == "asia-southeast1" && output.location == "asia-southeast1-b" && output.prefix == "tf-e2e-sgp"
    error_message = "Target outputs must match explicit inputs."
  }
}

run "reject_non_singapore_target" {
  command = plan

  variables {
    control_plane_cidr   = "10.12.0.0/28"
    project_id           = "sponsored-example-123"
    region               = "asia-southeast1"
    location             = "us-central1-a"
    prefix               = "tf-e2e-sgp"
    system_machine_type  = "n2-standard-2"
    primary_cidr         = "10.0.0.0/20"
    pods_cidr            = "10.4.0.0/14"
    services_cidr        = "10.8.0.0/20"
    github_repository_id = "863038341"
    github_owner_id      = "166281999"
  }

  expect_failures = [var.location]
}

run "restricted_wif" {
  command = plan

  variables {
    control_plane_cidr   = "10.12.0.0/28"
    project_id           = "sponsored-example-123"
    region               = "asia-southeast1"
    location             = "asia-southeast1-b"
    prefix               = "tf-e2e-sgp"
    system_machine_type  = "n2-standard-2"
    primary_cidr         = "10.0.0.0/20"
    pods_cidr            = "10.4.0.0/14"
    services_cidr        = "10.8.0.0/20"
    github_repository_id = "863038341"
    github_owner_id      = "166281999"
    enable_ci_wif        = true
  }

  assert {
    condition     = strcontains(google_iam_workload_identity_pool_provider.github[0].attribute_condition, "assertion.repository_owner_id == '166281999'") && strcontains(google_iam_workload_identity_pool_provider.github[0].attribute_condition, "assertion.workflow_ref == 'cloudpilot-ai/karpenter-provider-gcp/.github/workflows/e2e-manual.yaml@refs/heads/main'") && strcontains(google_iam_workload_identity_pool_provider.github[0].attribute_condition, "assertion.sub == 'repo:cloudpilot-ai/karpenter-provider-gcp:environment:e2e'")
    error_message = "WIF must restrict owner ID, trusted workflow and protected environment subject."
  }

  assert {
    condition     = !contains(google_project_iam_custom_role.runtime_reads.permissions, "iam.roles.update") && google_service_account_iam_member.controller_node_act_as.role == "roles/iam.serviceAccountUser"
    error_message = "Runtime cannot administer IAM; controller may only attach the dedicated node SA."
  }
}

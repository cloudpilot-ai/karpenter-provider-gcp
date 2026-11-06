mock_provider "google" {
  override_during = plan
  mock_data "google_project" {
    defaults = { number = "123456789012" }
  }
  mock_resource "google_project_iam_custom_role" {
    defaults = { id = "projects/offline-e2e-project/roles/offline_e2e_ci_runtime" }
  }
  mock_resource "google_artifact_registry_repository" {
    defaults = { name = "projects/offline-e2e-project/locations/us-central1/repositories/offline-e2e-images" }
  }
  mock_resource "google_tags_tag_key" {
    defaults = { name = "tagKeys/123456789" }
  }
  mock_resource "google_service_account" {
    defaults = { email = "offline-e2e-ci-sa@offline-e2e-project.iam.gserviceaccount.com", name = "projects/offline-e2e-project/serviceAccounts/offline-e2e-ci-sa@offline-e2e-project.iam.gserviceaccount.com" }
  }
  mock_resource "google_iam_workload_identity_pool" {
    defaults = { name = "projects/123456789012/locations/global/workloadIdentityPools/offline-e2e-ci" }
  }
}

variables {
  project_id            = "offline-e2e-project"
  region                = "us-central1"
  location              = "us-central1"
  node_locations        = ["us-central1-a"]
  system_node_locations = ["us-central1-a"]
  prefix                = "offline-e2e"
  system_machine_type   = "n2-standard-2"
  primary_cidr          = "10.0.0.0/24"
  pods_cidr             = "10.1.0.0/16"
  services_cidr         = "10.2.0.0/20"
  control_plane_cidr    = "10.3.0.0/28"
}

run "ci_is_default_off" {
  command = plan

  assert {
    condition = (
      length(google_service_account.ci_runtime) == 0 &&
      length(google_iam_workload_identity_pool.ci) == 0 &&
      length(google_iam_workload_identity_pool_provider.ci) == 0 &&
      length(google_service_account_iam_member.ci_federation) == 0 &&
      length(google_project_iam_custom_role.ci_runtime) == 0 &&
      length(google_project_iam_member.ci_runtime) == 0 &&
      length(google_artifact_registry_repository_iam_member.ci_push) == 0 &&
      length(google_tags_tag_key_iam_member.ci_read) == 0 &&
      output.ci_environment_variables == null && output.ci_workload_identity_provider == null &&
      output.ci_oidc_audience == null && output.ci_runtime_service_account == null
    )
    error_message = "Manual targets must not gain CI identities by default."
  }
}

run "runtime_grants_are_separate_and_scoped" {
  command = plan
  variables {
    ci_enabled            = true
    ci_pod_range_names    = { small = "small", large = "large" }
    additional_pod_ranges = { small = "10.4.0.0/22", large = "10.5.0.0/20" }
  }

  assert {
    condition     = google_project_iam_member.ci_runtime[0].role == google_project_iam_custom_role.ci_runtime[0].id && google_project_iam_custom_role.ci_runtime[0].role_id != var.controller_role_id
    error_message = "CI must use its own read-only role, not the controller role."
  }
  assert {
    condition     = google_artifact_registry_repository_iam_member.ci_push[0].repository == google_artifact_registry_repository.images.name && google_artifact_registry_repository_iam_member.ci_push[0].role == "roles/artifactregistry.writer"
    error_message = "Image writes must be scoped to the target repository."
  }
  assert {
    condition     = google_tags_tag_key_iam_member.ci_read[0].tag_key == google_tags_tag_key.e2e.name && google_tags_tag_key_iam_member.ci_read[0].role == "roles/resourcemanager.tagViewer"
    error_message = "Tag metadata access must be scoped to the fixture key."
  }
}

run "configuration_maps_explicit_fixture_keys" {
  command = plan
  variables {
    ci_enabled            = true
    ci_pod_range_names    = { small = "selected-small", large = "selected-large" }
    additional_pod_ranges = { selected-small = "10.4.0.0/22", selected-large = "10.5.0.0/20", unrelated = "10.6.0.0/24" }
  }
  assert {
    condition     = output.ci_environment_variables.E2E_SMALL_PODS_RANGE_NAME == "selected-small" && output.ci_environment_variables.E2E_LARGE_PODS_RANGE_NAME == "selected-large" && output.ci_environment_variables.E2E_PODS_RANGE_NAME == "offline-e2e-pods"
    error_message = "Use explicit additional range keys, never map order."
  }
  assert {
    condition     = output.ci_environment_variables.E2E_RESOURCE_MANAGER_TAG_KEY == "offline-e2e-tag" && output.resource_manager_tag_key.id == google_tags_tag_key.e2e.id && output.additional_pod_ranges == var.additional_pod_ranges
    error_message = "Export the tag resource plus test short name and the existing additional range map."
  }
}

run "ci_rejects_missing_range_selection" {
  command = plan
  variables { ci_enabled = true }
  expect_failures = [var.ci_pod_range_names]
}

run "ci_rejects_nonmember_range" {
  command = plan
  variables {
    ci_enabled            = true
    ci_pod_range_names    = { small = "not-present", large = "large" }
    additional_pod_ranges = { small = "10.4.0.0/22", large = "10.5.0.0/20" }
  }
  expect_failures = [var.ci_pod_range_names]
}

run "ci_rejects_reversed_capacity" {
  command = plan
  variables {
    ci_enabled            = true
    ci_pod_range_names    = { small = "small", large = "large" }
    additional_pod_ranges = { small = "10.4.0.0/20", large = "10.5.0.0/22" }
  }
  expect_failures = [var.ci_pod_range_names]
}

run "ci_cannot_overwrite_controller_role" {
  command = plan
  variables {
    ci_enabled            = true
    controller_role_id    = "offline_e2e_ci_runtime"
    ci_pod_range_names    = { small = "small", large = "large" }
    additional_pod_ranges = { small = "10.4.0.0/22", large = "10.5.0.0/20" }
  }
  expect_failures = [var.ci_enabled]
}

run "trust_uses_only_exact_upstream_dispatch" {
  command = plan
  variables {
    ci_enabled            = true
    ci_pod_range_names    = { small = "small", large = "large" }
    additional_pod_ranges = { small = "10.4.0.0/22", large = "10.5.0.0/20" }
  }
  assert {
    condition = (
      google_iam_workload_identity_pool_provider.ci[0].oidc[0].issuer_uri == "https://token.actions.githubusercontent.com" &&
      google_iam_workload_identity_pool_provider.ci[0].oidc[0].allowed_audiences == tolist(["https://iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/offline-e2e-ci/providers/github"]) &&
      output.ci_oidc_audience == google_iam_workload_identity_pool_provider.ci[0].oidc[0].allowed_audiences[0] &&
      output.ci_workload_identity_provider == "projects/123456789012/locations/global/workloadIdentityPools/offline-e2e-ci/providers/github"
    )
    error_message = "Pin issuer and exactly one explicit provider audience; export that audience to auth."
  }
  assert {
    condition = (
      google_iam_workload_identity_pool_provider.ci[0].attribute_condition == trimspace(file("ci-oidc-condition.cel")) &&
      google_iam_workload_identity_pool_provider.ci[0].attribute_mapping == tomap({
        "google.subject"                = "assertion.sub"
        "attribute.repository_id"       = "assertion.repository_id"
        "attribute.repository_owner_id" = "assertion.repository_owner_id"
        "attribute.ref"                 = "assertion.ref"
        "attribute.workflow_ref"        = "assertion.workflow_ref"
        "attribute.event_name"          = "assertion.event_name"
        "attribute.environment"         = "assertion.environment"
      })
    )
    error_message = "Use the tested CEL and exact mapped claims, without reusable-workflow dependency."
  }
  assert {
    condition = (
      google_service_account_iam_member.ci_federation[0].role == "roles/iam.workloadIdentityUser" &&
      google_service_account_iam_member.ci_federation[0].service_account_id == google_service_account.ci_runtime[0].name &&
      google_service_account_iam_member.ci_federation[0].member == "principalSet://iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/offline-e2e-ci/attribute.repository_id/863038341"
    )
    error_message = "Only the exact mapped upstream repository in this dedicated pool may impersonate runtime."
  }
}

run "ci_rejects_same_range_key" {
  command = plan
  variables {
    ci_enabled            = true
    ci_pod_range_names    = { small = "small", large = "small" }
    additional_pod_ranges = { small = "10.4.0.0/22" }
  }
  expect_failures = [var.ci_pod_range_names]
}

run "ci_rejects_insufficient_small_range" {
  command = plan
  variables {
    ci_enabled            = true
    ci_pod_range_names    = { small = "small", large = "large" }
    additional_pod_ranges = { small = "10.4.0.0/23", large = "10.5.0.0/20" }
  }
  expect_failures = [var.ci_pod_range_names]
}

run "ci_rejects_ipv6_range" {
  command = plan
  variables {
    ci_enabled            = true
    ci_pod_range_names    = { small = "small", large = "large" }
    additional_pod_ranges = { small = "2001:db8::/64", large = "10.5.0.0/20" }
  }
  expect_failures = [var.ci_pod_range_names]
}

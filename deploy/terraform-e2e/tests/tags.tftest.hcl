mock_provider "google" {}

variables {
  project_id          = "example-e2e-project"
  region              = "asia-southeast1"
  location            = "asia-southeast1-b"
  prefix              = "tf-e2e-sgp"
  system_machine_type = "e2-standard-2"
  primary_cidr        = "10.0.0.0/20"
  pods_cidr           = "10.4.0.0/14"
  services_cidr       = "10.8.0.0/20"
  control_plane_cidr  = "10.12.0.0/28"
}

run "resource_manager_tag_fixture" {
  # Apply only against the mocked provider; no GCP calls are made.
  command = apply

  override_module {
    target = module.cluster
    outputs = {
      karpenter_controller_sa_email = "controller@example-e2e-project.iam.gserviceaccount.com"
      karpenter_node_sa_email       = "node@example-e2e-project.iam.gserviceaccount.com"
      network_id                    = "projects/example-e2e-project/global/networks/tf-e2e-sgp-vpc"
      cluster_name                  = "tf-e2e-sgp-cluster"
    }
  }

  override_resource {
    target = google_tags_tag_key.e2e
    values = { name = "tagKeys/123" }
  }

  assert {
    condition     = google_tags_tag_key.e2e.parent == "projects/example-e2e-project" && google_tags_tag_key.e2e.short_name == "tf-e2e-sgp-tag"
    error_message = "The fixture tag key must be project-scoped and match the runner's prefix."
  }

  assert {
    condition     = google_tags_tag_value.e2e.parent == "tagKeys/123" && google_tags_tag_value.e2e.short_name == "e2e"
    error_message = "The fixture value must match the resourceManagerTags tests."
  }

  assert {
    condition     = google_tags_tag_key_iam_member.controller.tag_key == "tagKeys/123" && google_tags_tag_key_iam_member.controller.role == "roles/resourcemanager.tagUser" && google_tags_tag_key_iam_member.controller.member == "serviceAccount:controller@example-e2e-project.iam.gserviceaccount.com"
    error_message = "Grant tagUser only to the controller on the fixture key."
  }
}

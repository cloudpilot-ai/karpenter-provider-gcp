mock_provider "google" {}

variables {
  project_id          = "example-e2e-project"
  region              = "asia-southeast1"
  location            = "asia-southeast1-b"
  prefix              = "tf-e2e-sgp"
  system_machine_type = "e2-standard-2"
  kubernetes_version  = "1.35"
  primary_cidr        = "10.0.0.0/20"
  pods_cidr           = "10.4.0.0/14"
  services_cidr       = "10.8.0.0/20"
  control_plane_cidr  = "10.12.0.0/28"
  additional_pod_ranges = {
    tf-e2e-sgp-pods-small = "10.13.0.0/24"
    tf-e2e-sgp-pods-large = "10.14.0.0/20"
  }
}

run "e2e_135" {
  command = plan
}

run "e2e_136" {
  command = plan

  variables {
    kubernetes_version = "1.36"
  }
}

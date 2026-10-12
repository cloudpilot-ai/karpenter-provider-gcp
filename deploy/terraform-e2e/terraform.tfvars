# Shared non-secret configuration for the persistent e2e environment.
project_id          = "planetscale-karp-gcp-e2e"
region              = "asia-southeast1"
location            = "asia-southeast1-b"
prefix              = "tf-e2e-sgp"
system_machine_type = "n2-standard-2"
release_channel     = "STABLE"
primary_cidr        = "10.0.0.0/20"
pods_cidr           = "10.4.0.0/14"
services_cidr       = "10.8.0.0/20"
control_plane_cidr  = "10.12.0.0/28"
additional_pod_ranges = {
  tf-e2e-sgp-pods-small = "10.13.0.0/22"
  tf-e2e-sgp-pods-large = "10.14.0.0/20"
}

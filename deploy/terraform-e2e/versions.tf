terraform {
  # Input validation blocks reference other variables (cross-variable validation),
  # which needs Terraform 1.9 or later.
  required_version = ">= 1.9.0"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "6.28.0"
    }
  }

  # Backends cannot read variables. The shared target's bucket and prefix live in
  # backend.hcl and are passed at init time, so a private environment can point at
  # its own bucket with -backend-config=private.backend.hcl or use local state.
  backend "gcs" {}
}

provider "google" {
  project = var.project_id
  region  = var.region
}

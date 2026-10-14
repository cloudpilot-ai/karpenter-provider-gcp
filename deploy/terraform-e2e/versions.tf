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

  # Backends cannot read variables, so this is the shared target's state. A
  # private environment overrides it at init with its own bucket:
  # terraform init -reconfigure -backend-config=private.gcs.tfbackend
  backend "gcs" {
    bucket = "planetscale-karp-gcp-e2e-tfstate"
    prefix = "karpenter/e2e"
  }
}

provider "google" {
  project = var.project_id
  region  = var.region
}

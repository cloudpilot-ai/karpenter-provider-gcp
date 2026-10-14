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

  backend "gcs" {
    bucket = "planetscale-karp-gcp-e2e-tfstate"
    prefix = "karpenter/e2e"
  }
}

provider "google" {
  project = var.project_id
  region  = var.region
}

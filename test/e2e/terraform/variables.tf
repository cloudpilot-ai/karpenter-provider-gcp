variable "project_id" {
  description = "GCP project ID used for the e2e cluster."
  type        = string
}

variable "region" {
  description = "Regional location for the GKE cluster."
  type        = string
  default     = "us-central1"
}

variable "prefix" {
  description = "Prefix applied to all e2e resources."
  type        = string
}

variable "credentials_path" {
  description = "Absolute path to the GCP service account credentials JSON file."
  type        = string
}

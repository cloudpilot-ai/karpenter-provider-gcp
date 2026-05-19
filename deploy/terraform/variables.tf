variable "common_name" {
  type    = string
  default = "karpenter-provider-gcp"
  validation {
    condition     = length(var.common_name) <= 25
    error_message = "common_name must be at most 25 characters (service account IDs are suffixed with -ctrl/-node and must fit GCP's 30-character limit)."
  }
}

variable "google_region" {
  type    = string
  default = "us-central1"
}

variable "project_id" {
  type    = string
  default = "karpenter-provider-gcp"
}

variable "kubernetes_namespace" {
  type        = string
  default     = "karpenter-system"
  description = "Kubernetes namespace where the Karpenter controller runs."
}

variable "kubernetes_service_account" {
  type        = string
  default     = "karpenter"
  description = "Kubernetes service account name for the Karpenter controller."
}

variable "node_service_account_email" {
  type        = string
  description = "Email of an existing GCP SA to attach to provisioned nodes. When empty, the module creates a dedicated node SA (karpenter_node) with roles/container.nodeServiceAccount."
  default     = ""
}

variable "bind_artifactregistry_reader" {
  type        = bool
  default     = false
  description = "Bind roles/artifactregistry.reader to the node SA at the project level. Set to true when nodes pull images from a project-owned Artifact Registry repository. Has no effect when node_service_account_email is provided (BYOSA manages its own permissions)."
}

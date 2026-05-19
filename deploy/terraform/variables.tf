variable "common_name" {
  type    = string
  default = "karpenter-provider-gcp"
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

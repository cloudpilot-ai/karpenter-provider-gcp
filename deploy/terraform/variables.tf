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

variable "network_name" {
  type        = string
  default     = null
  description = "VPC name; defaults to common_name."
}

variable "subnetwork_name" {
  type        = string
  default     = null
  description = "Subnet name; defaults to common_name."
}

variable "cluster_name" {
  type        = string
  default     = null
  description = "Cluster name; defaults to common_name for standalone installs."
}

variable "cluster_location" {
  type        = string
  default     = null
  description = "GKE zone or region; defaults to google_region."
}

variable "kubernetes_version" {
  type        = string
  default     = null
  description = "Minimum GKE master version (minor such as 1.35 or full GKE version); null uses GKE's default. Auto-upgrades still apply."
}

variable "primary_cidr" {
  type    = string
  default = "10.0.0.0/24"
}

variable "services_cidr" {
  type    = string
  default = "10.10.0.0/22"
}

variable "pods_cidr" {
  type    = string
  default = "10.100.0.0/20"
}

variable "services_range_name" {
  type    = string
  default = "services-range"
}

variable "pods_range_name" {
  type    = string
  default = "pod-ranges"
}

variable "additional_pod_ranges" {
  type        = map(string)
  default     = {}
  description = "Additional GKE pod secondary ranges, keyed by subnet range name. CIDRs must not overlap other ranges."
  validation {
    condition     = alltrue([for cidr in values(var.additional_pod_ranges) : can(cidrhost(cidr, 0))])
    error_message = "Each additional pod range must have a valid CIDR."
  }
}

variable "private_nodes" {
  type    = bool
  default = false
}

variable "control_plane_cidr" {
  type        = string
  default     = null
  description = "Non-overlapping /28 required when private_nodes is true."
}

variable "deletion_protection" {
  type    = bool
  default = false
}

variable "controller_service_account_id" {
  type        = string
  default     = null
  description = "Controller identity name; defaults to common_name-ctrl."
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

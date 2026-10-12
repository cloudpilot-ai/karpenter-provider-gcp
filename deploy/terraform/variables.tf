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

variable "system_machine_type" {
  type        = string
  default     = "n2-standard-2"
  nullable    = false
  description = "Machine type for the persistent pool hosting Karpenter and cluster system workloads."
  validation {
    condition     = length(trimspace(var.system_machine_type)) > 0
    error_message = "Set a non-empty system machine type."
  }
}

variable "system_node_count" {
  type        = number
  default     = 1
  nullable    = false
  description = "System nodes per zone; regional clusters multiply this count by their node locations."
  validation {
    condition     = var.system_node_count >= 1 && floor(var.system_node_count) == var.system_node_count
    error_message = "System node count must be a positive integer."
  }
}

variable "system_disk_size_gb" {
  type        = number
  default     = 30
  nullable    = false
  description = "Boot disk size in GB for each system node."
  validation {
    condition     = var.system_disk_size_gb >= 10 && floor(var.system_disk_size_gb) == var.system_disk_size_gb
    error_message = "System disk size must be an integer of at least 10 GB."
  }
}

variable "release_channel" {
  type        = string
  default     = "STABLE"
  nullable    = false
  description = "GKE release channel; GKE selects and auto-upgrades the Kubernetes version."
  validation {
    condition     = contains(["STABLE", "REGULAR", "RAPID", "EXTENDED"], var.release_channel)
    error_message = "Select STABLE, REGULAR, RAPID or EXTENDED."
  }
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

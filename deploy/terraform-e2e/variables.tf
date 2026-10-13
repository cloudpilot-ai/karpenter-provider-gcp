variable "project_id" {
  type        = string
  description = "Existing sponsored GCP project; never use a local-test project."
  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.project_id))
    error_message = "Set an explicit valid GCP project ID."
  }
}

variable "region" {
  type        = string
  description = "Approved Singapore region."
  validation {
    condition     = var.region == "asia-southeast1"
    error_message = "The sponsored e2e target must be in asia-southeast1."
  }
}

variable "location" {
  type        = string
  description = "Regional GKE control-plane location."
  validation {
    condition     = var.location == "asia-southeast1"
    error_message = "The shared e2e cluster must be regional in asia-southeast1."
  }
}

variable "node_locations" {
  type        = list(string)
  default     = ["asia-southeast1-a", "asia-southeast1-b", "asia-southeast1-c"]
  nullable    = false
  description = "Default worker zones available to Karpenter; the persistent system pool remains in asia-southeast1-b."
  validation {
    condition     = length(var.node_locations) > 0 && alltrue([for zone in var.node_locations : can(regex("^asia-southeast1-[abc]$", zone))])
    error_message = "Select at least one worker zone in asia-southeast1."
  }
}

variable "prefix" {
  type        = string
  description = "Shared e2e resource prefix, matching the former setup script."
  validation {
    condition     = var.prefix == "karpenter-e2e"
    error_message = "Use the shared karpenter-e2e resource names."
  }
}

variable "system_machine_type" {
  type        = string
  description = "Approved system-pool machine type."
  validation {
    condition     = length(trimspace(var.system_machine_type)) > 0
    error_message = "Set a reviewed system machine type."
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

variable "additional_pod_ranges" {
  type        = map(string)
  default     = {}
  description = "Additional named pod CIDRs; include small and large ranges for multi-range networking tests. CIDRs must not overlap other ranges."
}

variable "primary_cidr" {
  type        = string
  description = "Approved subnet primary CIDR (must not overlap other VPC ranges)."
  validation {
    condition     = can(cidrhost(var.primary_cidr, 0))
    error_message = "Set an approved primary CIDR."
  }
}

variable "pods_cidr" {
  type        = string
  description = "Approved GKE pods secondary CIDR."
  validation {
    condition     = can(cidrhost(var.pods_cidr, 0))
    error_message = "Set an approved pods CIDR."
  }
}

variable "services_cidr" {
  type        = string
  description = "Approved GKE services secondary CIDR."
  validation {
    condition     = can(cidrhost(var.services_cidr, 0))
    error_message = "Set an approved services CIDR."
  }
}

variable "control_plane_cidr" {
  type        = string
  description = "Approved, non-overlapping /28 for GKE private-node control plane peering."
  validation {
    condition     = can(cidrhost(var.control_plane_cidr, 0)) && can(regex("/28$", var.control_plane_cidr))
    error_message = "Set a reviewed non-overlapping /28 control-plane CIDR."
  }
}

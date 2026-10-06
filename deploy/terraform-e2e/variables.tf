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
  description = "Approved Singapore GKE zone (verify GPU/ARM availability and quota before provisioning)."
  validation {
    condition     = can(regex("^asia-southeast1-[abc]$", var.location))
    error_message = "Set an approved zone in asia-southeast1."
  }
}

variable "prefix" {
  type        = string
  description = "Dedicated e2e resource prefix, distinct from local test infrastructure."
  validation {
    condition     = can(regex("^tf-e2e-[a-z0-9-]{1,9}[a-z0-9]$", var.prefix))
    error_message = "Use the reserved tf-e2e- prefix with a 2–10 character lowercase suffix (at most 18 characters total)."
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

variable "kubernetes_version" {
  type        = string
  nullable    = false
  description = "Requested minimum GKE master version, such as 1.35 or 1.36 (must be available in the approved location). Auto-upgrades still apply."
  validation {
    condition     = length(trimspace(var.kubernetes_version)) > 0
    error_message = "Set an explicit Kubernetes minor or full GKE version."
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

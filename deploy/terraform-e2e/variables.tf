variable "project_id" {
  type        = string
  description = "GCP project for this e2e environment. The sponsored project is reserved for the shared target; use your own project for a private environment."
  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.project_id))
    error_message = "Set an explicit valid GCP project ID."
  }
}

variable "region" {
  type        = string
  description = "GCP region for the e2e environment."
  validation {
    condition     = can(regex("^[a-z]+-[a-z]+[0-9]+$", var.region))
    error_message = "Set a valid GCP region."
  }
}

variable "location" {
  type        = string
  description = "GKE control-plane location, either a region or a zone."
  validation {
    condition     = can(regex("^[a-z]+-[a-z]+[0-9]+(-[a-z])?$", var.location))
    error_message = "Set a valid GCP region or zone."
  }
}

variable "node_locations" {
  type        = list(string)
  default     = ["asia-southeast1-a", "asia-southeast1-b", "asia-southeast1-c"]
  nullable    = false
  description = "Default worker zones available to Karpenter."
  validation {
    condition     = length(var.node_locations) > 0 && alltrue([for zone in var.node_locations : can(regex("^[a-z]+-[a-z]+[0-9]+-[a-z]$", zone))])
    error_message = "Select at least one valid GCP worker zone."
  }
}

variable "system_node_locations" {
  type        = list(string)
  default     = ["asia-southeast1-b"]
  nullable    = false
  description = "Zones for the persistent system node pool; one node is created per zone."
  validation {
    condition     = length(var.system_node_locations) > 0 && alltrue([for zone in var.system_node_locations : can(regex("^[a-z]+-[a-z]+[0-9]+-[a-z]$", zone))])
    error_message = "Select at least one valid GCP system-pool zone."
  }
  # GKE requires pool zones to be part of the cluster's node locations.
  validation {
    condition     = alltrue([for zone in var.system_node_locations : contains(var.node_locations, zone)])
    error_message = "Every system-pool zone must also appear in node_locations."
  }
}

variable "prefix" {
  type        = string
  description = "Resource name prefix. Use a name unique to your project unless you are the shared target."
  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{0,19}$", var.prefix))
    error_message = "Use a lowercase resource prefix starting with a letter and no longer than 20 characters."
  }
  validation {
    condition     = var.project_id != "planetscale-karp-gcp-e2e" || var.prefix == "karpenter-e2e"
    error_message = "The sponsored project is reserved for the shared karpenter-e2e target; run a private environment in your own project."
  }
}

variable "controller_role_id" {
  type        = string
  default     = "karpenter_controller"
  description = "Custom project role ID for the controller. Use a distinct ID when another environment already owns this name in the same project."
  validation {
    condition     = can(regex("^[a-zA-Z][a-zA-Z0-9_.]{2,63}$", var.controller_role_id))
    error_message = "Use a valid custom role ID."
  }
}

variable "datapath_provider" {
  type        = string
  default     = "ADVANCED_DATAPATH"
  description = "Cluster datapath: ADVANCED_DATAPATH for GKE Dataplane V2, or LEGACY_DATAPATH for kube-proxy."
  validation {
    condition     = contains(["ADVANCED_DATAPATH", "LEGACY_DATAPATH"], var.datapath_provider)
    error_message = "Select ADVANCED_DATAPATH or LEGACY_DATAPATH."
  }
}

variable "cloud_observability_enabled" {
  type        = bool
  default     = false
  description = "Enable GKE-managed Cloud Logging and Cloud Monitoring components and managed Prometheus for this environment."
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

variable "ci_enabled" {
  type        = bool
  default     = false
  nullable    = false
  description = "Opt in to upstream manual CI runtime IAM/WIF. Does not enable workflow runs or provision Kubernetes bootstrap."
  validation {
    condition     = !var.ci_enabled || var.controller_role_id != "${replace(var.prefix, "-", "_")}_ci_runtime"
    error_message = "The CI custom role ID must not collide with controller_role_id."
  }
}

variable "ci_pod_range_names" {
  type = object({
    small = string
    large = string
  })
  default     = null
  description = "Explicit existing additional_pod_ranges keys for CI. Required when ci_enabled; small must be IPv4 /22 or richer, large strictly richer. No ranges are inferred or created by CI."
  validation {
    condition     = !var.ci_enabled || var.ci_pod_range_names != null
    error_message = "CI requires explicit small and large additional pod range keys."
  }
  validation {
    condition = !var.ci_enabled || try(
      contains(keys(var.additional_pod_ranges), var.ci_pod_range_names.small) &&
      contains(keys(var.additional_pod_ranges), var.ci_pod_range_names.large), false
    )
    error_message = "CI range keys must be members of the existing additional_pod_ranges map."
  }
  validation {
    condition = !var.ci_enabled || try(
      var.ci_pod_range_names.small != var.ci_pod_range_names.large &&
      can(cidrnetmask(var.additional_pod_ranges[var.ci_pod_range_names.small])) &&
      can(cidrnetmask(var.additional_pod_ranges[var.ci_pod_range_names.large])) &&
      tonumber(split("/", var.additional_pod_ranges[var.ci_pod_range_names.small])[1]) <= 22 &&
      tonumber(split("/", var.additional_pod_ranges[var.ci_pod_range_names.small])[1]) > tonumber(split("/", var.additional_pod_ranges[var.ci_pod_range_names.large])[1]), false
    )
    error_message = "Select distinct IPv4 ranges: small must be /22 or richer and large strictly richer than small."
  }
}

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

# By default, Karpenter attaches the Compute Engine default SA to provisioned nodes.
# You can override this with a dedicated minimal-privilege node SA via GCENodeClass.spec.serviceAccount
# or the --node-pool-service-account controller flag. Providing a custom SA here binds
# iam.serviceAccountUser on that SA for the controller; repeat for each SA you use.
# Recommended: create a dedicated node SA with only the permissions your workloads need
# rather than relying on the broad Compute Engine default SA.
variable "node_service_account_email" {
  type        = string
  description = "Email of the GCP SA attached to provisioned nodes (iam.serviceAccountUser is bound here). Defaults to the Compute Engine default SA if left empty — set this to a minimal-privilege SA instead."
  default     = ""
}

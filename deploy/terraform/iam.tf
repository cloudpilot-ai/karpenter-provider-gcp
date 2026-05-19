locals {
  iam_role      = yamldecode(file("${path.module}/../iam/karpenter-controller-role.yaml"))
  node_sa_email = var.node_service_account_email != "" ? var.node_service_account_email : google_service_account.karpenter_node[0].email
}

resource "google_service_account" "karpenter_controller" {
  account_id   = "${var.common_name}-ctrl"
  display_name = "Karpenter controller"
  project      = var.project_id
}

resource "google_service_account" "karpenter_node" {
  count        = var.node_service_account_email == "" ? 1 : 0
  account_id   = "${var.common_name}-node"
  display_name = "Karpenter node"
  project      = var.project_id
}

resource "google_project_iam_member" "karpenter_node" {
  count   = var.node_service_account_email == "" ? 1 : 0
  project = var.project_id
  role    = "roles/container.nodeServiceAccount"
  member  = "serviceAccount:${google_service_account.karpenter_node[0].email}"
}

resource "google_project_iam_custom_role" "karpenter_controller" {
  role_id     = "${replace(var.common_name, "-", "_")}_controller"
  title       = local.iam_role.title
  description = local.iam_role.description
  permissions = local.iam_role.includedPermissions
  project     = var.project_id
}

resource "google_project_iam_member" "karpenter_controller" {
  project = var.project_id
  role    = google_project_iam_custom_role.karpenter_controller.id
  member  = "serviceAccount:${google_service_account.karpenter_controller.email}"
}

resource "google_service_account_iam_member" "workload_identity" {
  service_account_id = google_service_account.karpenter_controller.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[${var.kubernetes_namespace}/${var.kubernetes_service_account}]"
}

# Bind iam.serviceAccountUser on the node SA so the controller can attach it to VMs.
resource "google_service_account_iam_member" "node_sa_actAs" {
  service_account_id = "projects/${var.project_id}/serviceAccounts/${local.node_sa_email}"
  role               = "roles/iam.serviceAccountUser"
  member             = "serviceAccount:${google_service_account.karpenter_controller.email}"
}

output "karpenter_controller_sa_email" {
  value = google_service_account.karpenter_controller.email
}

output "karpenter_node_sa_email" {
  value = local.node_sa_email
}

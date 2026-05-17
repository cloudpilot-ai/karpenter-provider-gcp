locals {
  iam_role = yamldecode(file("${path.module}/../iam/karpenter-controller-role.yaml"))
}

resource "google_service_account" "karpenter_controller" {
  account_id   = "${var.common_name}-ctrl"
  display_name = "Karpenter controller"
  project      = var.project_id
}

resource "google_project_iam_custom_role" "karpenter_controller" {
  role_id     = "karpenter_controller"
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
# If node_service_account_email is empty the Compute Engine default SA is used and no
# explicit binding is needed (the controller already has compute.projects.get to discover it).
resource "google_service_account_iam_member" "node_sa_actAs" {
  count              = var.node_service_account_email != "" ? 1 : 0
  service_account_id = "projects/${var.project_id}/serviceAccounts/${var.node_service_account_email}"
  role               = "roles/iam.serviceAccountUser"
  member             = "serviceAccount:${google_service_account.karpenter_controller.email}"
}

output "karpenter_controller_sa_email" {
  value = google_service_account.karpenter_controller.email
}

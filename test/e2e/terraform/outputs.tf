output "cluster_name" {
  description = "Created GKE cluster name."
  value       = google_container_cluster.e2e.name
}

output "cluster_endpoint" {
  description = "Created GKE cluster endpoint."
  value       = google_container_cluster.e2e.endpoint
}

output "kubeconfig_path" {
  description = "Local kubeconfig file used by the e2e suite."
  value       = pathexpand("~/.kube/config")
}

output "karpenter_sa_email" {
  description = "Workload Identity Google service account for the Karpenter controller."
  value       = google_service_account.karpenter.email
}

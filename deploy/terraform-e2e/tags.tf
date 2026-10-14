resource "google_tags_tag_key" "e2e" {
  parent      = "projects/${var.project_id}"
  short_name  = "${var.prefix}-tag"
  description = "Karpenter e2e resourceManagerTags"
}

resource "google_tags_tag_value" "e2e" {
  parent     = google_tags_tag_key.e2e.id
  short_name = "e2e"
}

resource "google_tags_tag_key_iam_member" "controller" {
  tag_key = google_tags_tag_key.e2e.name
  role    = "roles/resourcemanager.tagUser"
  member  = "serviceAccount:${module.cluster.karpenter_controller_sa_email}"
}

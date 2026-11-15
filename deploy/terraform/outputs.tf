# Outputs consumed by `podserve init --config` (docs/design/platform.md "Terraform outputs"): it
# reads `terraform output -json` and fills the podserve-platform ConfigMap from these.

output "project_id" {
  description = "The project everything was created in."
  value       = var.project_id
}

output "region" {
  description = "The region the cluster, bucket and repositories live in."
  value       = var.region
}

output "model_bucket" {
  description = "The GCS bucket holding the fetched weights."
  value       = google_storage_bucket.models.name
}

output "dockerhub_mirror" {
  description = "The Docker Hub mirror; pull vLLM from here."
  value       = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.dockerhub.repository_id}"
}

output "images_repo" {
  description = "The repository podserve's own images are pushed to."
  value       = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.images.repository_id}"
}

output "hf_token_secret" {
  description = "The HF-token secret, with no version; the user adds the token (D-014)."
  value       = google_secret_manager_secret.hf_token.id
}

output "build_service_account" {
  description = "The service account Cloud Build builds the fetch image with (D-023)."
  value       = google_service_account.build.email
}

output "cluster_name" {
  description = "The GKE cluster to point kubectl at."
  value       = google_container_cluster.podserve.name
}

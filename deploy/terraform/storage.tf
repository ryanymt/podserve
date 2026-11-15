# Weights live in GCS, containers in Artifact Registry. The Docker Hub mirror caches vLLM's public
# images so no node ever pulls from Docker Hub directly. The HF-token secret is created empty: the
# user adds the token out of band (D-014), so no token ever lands in state or logs.

resource "google_storage_bucket" "models" {
  name          = "${var.project_id}-${var.name}-models"
  location      = var.region
  force_destroy = true # AC1: terraform destroy removes the weights too

  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"

  soft_delete_policy {
    retention_duration_seconds = 0 # deleted weights must not keep billing
  }

  depends_on = [google_project_service.services]
}

resource "google_artifact_registry_repository" "dockerhub" {
  location      = var.region
  repository_id = "${var.name}-dockerhub"
  format        = "DOCKER"
  mode          = "REMOTE_REPOSITORY"

  remote_repository_config {
    docker_repository {
      public_repository = "DOCKER_HUB"
    }
  }

  depends_on = [google_project_service.services]
}

resource "google_artifact_registry_repository" "images" {
  location      = var.region
  repository_id = var.name
  format        = "DOCKER"
  mode          = "STANDARD_REPOSITORY"

  depends_on = [google_project_service.services]
}

resource "google_secret_manager_secret" "hf_token" {
  secret_id = "podserve-hf-token"

  replication {
    auto {}
  }

  depends_on = [google_project_service.services]
}

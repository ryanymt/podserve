# IAM (D-013, D-023). Workloads are Workload Identity Federation principals — the Kubernetes service
# accounts podserve/podserve-fetch and podserve/podserve-model — with no Google service account and
# no key. Two Google service accounts remain, for infrastructure only: the GKE node account and
# Cloud Build's. `roles/storage.legacyBucketReader` on the model bucket is required: Run:ai's GCS
# client calls get_bucket() before listing files, which no object role allows (found live in T0).

# --- Workload Identity Federation principals ------------------------------------------------

resource "google_storage_bucket_iam_member" "workloads" {
  for_each = {
    fetch-admin  = { member = "${local.wif}/podserve-fetch", role = "roles/storage.objectAdmin" }
    fetch-reader = { member = "${local.wif}/podserve-fetch", role = "roles/storage.legacyBucketReader" }
    model-viewer = { member = "${local.wif}/podserve-model", role = "roles/storage.objectViewer" }
    model-reader = { member = "${local.wif}/podserve-model", role = "roles/storage.legacyBucketReader" }
  }

  bucket = google_storage_bucket.models.name
  role   = each.value.role
  member = each.value.member

  depends_on = [google_project_service.services]
}

resource "google_secret_manager_secret_iam_member" "fetch" {
  secret_id = google_secret_manager_secret.hf_token.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "${local.wif}/podserve-fetch"

  depends_on = [google_project_service.services]
}

# --- The node service account -----------------------------------------------------------------

resource "google_service_account" "nodes" {
  account_id   = "${var.name}-nodes"
  display_name = "podserve GKE nodes"

  depends_on = [google_project_service.services]
}

resource "google_project_iam_member" "nodes" {
  for_each = toset([
    "roles/container.defaultNodeServiceAccount",
    "roles/logging.logWriter",
    "roles/monitoring.metricWriter",
    "roles/monitoring.viewer",
    "roles/autoscaling.metricsWriter",
  ])

  project = var.project_id
  role    = each.key
  member  = "serviceAccount:${google_service_account.nodes.email}"

  depends_on = [google_project_service.services]
}

resource "google_artifact_registry_repository_iam_member" "nodes_reader" {
  for_each = {
    dockerhub = google_artifact_registry_repository.dockerhub.repository_id
    images    = google_artifact_registry_repository.images.repository_id
  }

  location   = var.region
  repository = each.value
  role       = "roles/artifactregistry.reader"
  member     = "serviceAccount:${google_service_account.nodes.email}"

  depends_on = [google_project_service.services]
}

# --- Cloud Build (D-023) ------------------------------------------------------------------------

resource "google_service_account" "build" {
  account_id   = "${var.name}-build"
  display_name = "podserve Cloud Build"

  depends_on = [google_project_service.services]
}

resource "google_project_iam_member" "build_logs" {
  project = var.project_id
  role    = "roles/logging.logWriter"
  member  = "serviceAccount:${google_service_account.build.email}"

  depends_on = [google_project_service.services]
}

resource "google_artifact_registry_repository_iam_member" "build_writer" {
  location   = var.region
  repository = google_artifact_registry_repository.images.repository_id
  role       = "roles/artifactregistry.writer"
  member     = "serviceAccount:${google_service_account.build.email}"

  depends_on = [google_project_service.services]
}

# For the `_build/` source staging and logs prefix.
resource "google_storage_bucket_iam_member" "build" {
  bucket = google_storage_bucket.models.name
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${google_service_account.build.email}"

  depends_on = [google_project_service.services]
}

# `gcloud builds submit --gcs-source-staging-dir` also needs storage.buckets.get on the staging
# bucket, which no object role grants (found live in P01 T11, like Run:ai's get_bucket in T0).
resource "google_storage_bucket_iam_member" "build_bucket_reader" {
  bucket = google_storage_bucket.models.name
  role   = "roles/storage.legacyBucketReader"
  member = "serviceAccount:${google_service_account.build.email}"

  depends_on = [google_project_service.services]
}

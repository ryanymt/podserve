# The podserve platform (docs/design/platform.md): one model, one command. Everything is created in
# the user's own project, everything can be destroyed again (P01 AC1), and workloads get access via
# Workload Identity Federation principals — no service-account keys anywhere.

provider "google" {
  project = var.project_id
  region  = var.region
}

# The project number, needed to spell the Workload Identity Federation principals (D-013).
data "google_project" "this" {
  project_id = var.project_id
}

locals {
  # podserve's Kubernetes service accounts (names are constants, platform.md) as Workload Identity
  # Federation principals of the cluster's implicit workload identity pool: ns/podserve/sa/<KSA>.
  wif = "principal://iam.googleapis.com/projects/${data.google_project.this.number}/locations/global/workloadIdentityPools/${var.project_id}.svc.id.goog/subject/ns/podserve/sa"
}

resource "google_project_service" "services" {
  for_each = toset([
    "container.googleapis.com",
    "compute.googleapis.com",
    "artifactregistry.googleapis.com",
    "secretmanager.googleapis.com",
    "cloudbuild.googleapis.com",
    "iam.googleapis.com",
    "monitoring.googleapis.com",
    "storage.googleapis.com",
    "logging.googleapis.com",
  ])

  project            = var.project_id
  service            = each.key
  disable_on_destroy = false # destroying the platform must not disable the project's APIs
}

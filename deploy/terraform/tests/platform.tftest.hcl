# Spine test (P01 T2), parked under testdata/spine/terraform/. T8 moves it to
# deploy/terraform/tests/platform.tftest.hcl with `git mv`, unchanged. It plans the platform against
# a mock google provider (offline; tests/terraform.test.sh) and pins the settings `terraform fmt` and
# `validate` can't see: teardown flags, Private Google Access, Autopilot and private nodes, and the
# exact Workload Identity principals and roles (docs/design/platform.md "IAM").

mock_provider "google" {
  mock_data "google_project" {
    defaults = {
      number = "123456789012"
    }
  }
}

variables {
  project_id = "my-project"
  region     = "us-central1"
}

run "platform" {
  command = apply # mock provider: offline; computed values are generated

  # --- Teardown-friendly (AC1: terraform destroy removes everything) -------------------------
  assert {
    condition     = google_container_cluster.podserve.deletion_protection == false
    error_message = "the cluster must have deletion_protection = false"
  }
  assert {
    condition     = google_storage_bucket.models.force_destroy == true
    error_message = "the model bucket must have force_destroy = true"
  }
  assert {
    condition     = alltrue([for s in google_project_service.services : s.disable_on_destroy == false])
    error_message = "project services must have disable_on_destroy = false"
  }
  assert {
    condition = alltrue([for api in ["container.googleapis.com", "compute.googleapis.com", "artifactregistry.googleapis.com",
      "secretmanager.googleapis.com", "cloudbuild.googleapis.com", "iam.googleapis.com", "monitoring.googleapis.com",
    "storage.googleapis.com"] : contains(keys(google_project_service.services), api)])
    error_message = "google_project_service.services (for_each by API name) must include the APIs podserve needs"
  }

  # --- Cluster (D-006, D-008) -----------------------------------------------------------------
  assert {
    condition     = google_container_cluster.podserve.enable_autopilot == true && google_container_cluster.podserve.location == "us-central1"
    error_message = "the cluster must be a regional Autopilot cluster in var.region"
  }
  assert {
    condition     = google_container_cluster.podserve.private_cluster_config[0].enable_private_nodes == true
    error_message = "nodes must be private"
  }
  assert {
    condition     = google_container_cluster.podserve.control_plane_endpoints_config[0].dns_endpoint_config[0].allow_external_traffic == true
    error_message = "the DNS-based control-plane endpoint must be enabled"
  }
  assert {
    condition     = google_compute_subnetwork.podserve.private_ip_google_access == true
    error_message = "the subnet must have Private Google Access"
  }
  assert {
    condition     = google_compute_router_nat.podserve.nat_ip_allocate_option == "AUTO_ONLY"
    error_message = "Cloud NAT must exist for reaching Hugging Face"
  }

  # --- Storage and registries ---------------------------------------------------------------------
  assert {
    condition     = google_storage_bucket.models.uniform_bucket_level_access == true && google_storage_bucket.models.public_access_prevention == "enforced"
    error_message = "the model bucket must use uniform access and enforce public access prevention"
  }
  assert {
    condition     = google_storage_bucket.models.soft_delete_policy[0].retention_duration_seconds == 0
    error_message = "the model bucket must have soft delete off (deleted weights must not be billed)"
  }
  assert {
    condition     = google_artifact_registry_repository.dockerhub.mode == "REMOTE_REPOSITORY" && google_artifact_registry_repository.dockerhub.remote_repository_config[0].docker_repository[0].public_repository == "DOCKER_HUB"
    error_message = "google_artifact_registry_repository.dockerhub must be a remote repository mirroring Docker Hub"
  }
  assert {
    condition     = google_artifact_registry_repository.images.mode == "STANDARD_REPOSITORY" && google_artifact_registry_repository.images.format == "DOCKER"
    error_message = "google_artifact_registry_repository.images must be a standard Docker repository"
  }
  assert {
    condition     = google_secret_manager_secret.hf_token.secret_id == "podserve-hf-token"
    error_message = "the HF token secret must be podserve-hf-token, with no version created by Terraform (D-014)"
  }

  # --- Workload Identity Federation principals (D-013) -------------------------------------------
  assert {
    condition = toset([for m in google_storage_bucket_iam_member.workloads : "${m.role} ${m.member}"]) == toset([
      "roles/storage.objectAdmin principal://iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/my-project.svc.id.goog/subject/ns/podserve/sa/podserve-fetch",
      "roles/storage.legacyBucketReader principal://iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/my-project.svc.id.goog/subject/ns/podserve/sa/podserve-fetch",
      "roles/storage.objectViewer principal://iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/my-project.svc.id.goog/subject/ns/podserve/sa/podserve-model",
      "roles/storage.legacyBucketReader principal://iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/my-project.svc.id.goog/subject/ns/podserve/sa/podserve-model",
    ])
    error_message = "google_storage_bucket_iam_member.workloads (for_each) must grant exactly these roles to the two KSAs"
  }
  assert {
    condition     = google_secret_manager_secret_iam_member.fetch.role == "roles/secretmanager.secretAccessor" && google_secret_manager_secret_iam_member.fetch.member == "principal://iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/my-project.svc.id.goog/subject/ns/podserve/sa/podserve-fetch"
    error_message = "podserve-fetch must be able to read the HF token secret"
  }

  # --- Outputs consumed by `podserve init --config` (platform.md) ----------------------------------
  assert {
    condition     = output.project_id == "my-project" && output.region == "us-central1" && output.hf_token_secret != null && output.model_bucket != null && output.cluster_name != null
    error_message = "outputs project_id, region, model_bucket, hf_token_secret and cluster_name must exist"
  }
}

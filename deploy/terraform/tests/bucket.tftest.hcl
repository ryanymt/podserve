# Parked spine test (P02 T2) for T8: `git mv` it to deploy/terraform/tests/bucket.tftest.hcl,
# unchanged. vLLM shows its --model path, gs://BUCKET/..., as "root" in /v1/models, so the model
# bucket's name must not contain the project ID (D-031). It is the platform name, "models" and the
# first 12 hex characters of sha256(project ID): fixed per project, so a re-apply keeps the bucket.

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

run "bucket_name" {
  command = plan

  assert {
    condition     = google_storage_bucket.models.name == "podserve-models-1c7cd944c28c"
    error_message = "the model bucket must be named podserve-models-<first 12 hex of sha256(project_id)>"
  }
  assert {
    condition     = !strcontains(google_storage_bucket.models.name, var.project_id)
    error_message = "the model bucket's name must not contain the project ID"
  }
}

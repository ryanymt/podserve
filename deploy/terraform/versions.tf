# Spine file (P01 T1): providers and versions are pinned here and in .terraform.lock.hcl. Build tasks
# must not edit this file or add providers or registry modules (workers have no network access).

terraform {
  required_version = ">= 1.14.0"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "8.8.1"
    }
  }

  # State lives in a GCS bucket the user creates once (D-016). Partial configuration: pass the bucket
  # at init, e.g.  terraform init -backend-config="bucket=<PROJECT>-podserve-tfstate"
  backend "gcs" {
    prefix = "podserve/platform"
  }
}

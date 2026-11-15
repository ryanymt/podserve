# Inputs. Copy terraform.tfvars.example to terraform.tfvars and set project_id (docs/quickstart.md).

variable "project_id" {
  description = "The user's own GCP project, where everything is created."
  type        = string
}

variable "region" {
  description = "Region of the cluster, bucket, Artifact Registry and Cloud NAT."
  type        = string
  default     = "us-central1"
}

variable "name" {
  description = "Short name that prefixes the cluster, repositories and service accounts."
  type        = string
  default     = "podserve"
}

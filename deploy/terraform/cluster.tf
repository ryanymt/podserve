# The private regional Autopilot cluster (D-006, D-008). Nodes have no public IP; the control plane
# is reached through its DNS endpoint, so there is no public IP to protect and no authorized
# networks to manage. deletion_protection is off so `terraform destroy` works (AC1).

resource "google_container_cluster" "podserve" {
  name     = var.name
  location = var.region

  network    = google_compute_network.podserve.id
  subnetwork = google_compute_subnetwork.podserve.id

  enable_autopilot    = true
  deletion_protection = false

  ip_allocation_policy {
    cluster_secondary_range_name  = "pods"
    services_secondary_range_name = "services"
  }

  private_cluster_config {
    enable_private_nodes = true
  }

  control_plane_endpoints_config {
    dns_endpoint_config {
      allow_external_traffic = true
    }
  }

  # Autopilot creates its node pools itself; this is where their service account is set.
  cluster_autoscaling {
    auto_provisioning_defaults {
      service_account = google_service_account.nodes.email
    }
  }

  release_channel {
    channel = "REGULAR"
  }

  depends_on = [google_project_service.services]
}

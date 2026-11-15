# A private network with Private Google Access (nodes reach Artifact Registry and Secret Manager
# without a public IP) and Cloud NAT (one way out, to Hugging Face and Docker Hub).

resource "google_compute_network" "podserve" {
  name                    = var.name
  auto_create_subnetworks = false

  depends_on = [google_project_service.services]
}

resource "google_compute_subnetwork" "podserve" {
  name          = "${var.name}-subnet"
  region        = var.region
  network       = google_compute_network.podserve.id
  ip_cidr_range = "10.10.0.0/20"

  private_ip_google_access = true

  secondary_ip_range {
    range_name    = "pods"
    ip_cidr_range = "10.32.0.0/14"
  }

  secondary_ip_range {
    range_name    = "services"
    ip_cidr_range = "10.36.0.0/20"
  }

  depends_on = [google_project_service.services]
}

resource "google_compute_router" "podserve" {
  name    = "${var.name}-router"
  region  = var.region
  network = google_compute_network.podserve.id

  depends_on = [google_project_service.services]
}

resource "google_compute_router_nat" "podserve" {
  name                               = "${var.name}-nat"
  region                             = var.region
  router                             = google_compute_router.podserve.name
  nat_ip_allocate_option             = "AUTO_ONLY"
  source_subnetwork_ip_ranges_to_nat = "ALL_SUBNETWORKS_ALL_IP_RANGES"

  depends_on = [google_project_service.services]
}

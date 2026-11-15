# The model-service dashboard (dashboards/model-service.json, T9's data), so the user can watch
# tokens per second, KV-cache pressure and errors without building charts by hand.

resource "google_monitoring_dashboard" "model_service" {
  dashboard_json = file("${path.module}/../../dashboards/model-service.json")

  depends_on = [google_project_service.services]
}

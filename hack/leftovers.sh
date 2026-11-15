#!/usr/bin/env bash
# List what's left in a GCP project after `terraform destroy` (the AC1 leftover check).
#
#   bash hack/leftovers.sh PROJECT [REGION]
#
# Expected by design: the Terraform state bucket <PROJECT>-podserve-tfstate (D-016), the project's
# enabled APIs, and the budget (D-020; it lives on the billing account and isn't listed here).
# Anything else is a leftover. Output is redacted for the record: project numbers and emails are
# replaced, and service accounts are listed by display name.
set -uo pipefail
P=${1:?usage: hack/leftovers.sh PROJECT [REGION]}
R=${2:-us-central1}
NUM=$(gcloud projects describe "$P" --format='value(projectNumber)' 2>/dev/null || true)

redact() {
  if [ -n "$NUM" ]; then sed -E -e "s/$NUM/PROJECT_NUMBER/g"; else cat; fi |
    sed -E -e 's/[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[a-z]{2,}/REDACTED_EMAIL/g' -e 's/^/  /'
}
run() { echo "\$ $*"; "$@" 2>&1 | redact; }

run gcloud container clusters list --project "$P" --format='value(name,location,status)'
run gcloud compute instances list --project "$P" --format='value(name,zone)'
run gcloud compute disks list --project "$P" --format='value(name,zone,sizeGb)'
run gcloud compute addresses list --project "$P" --format='value(name,region,address)'
run gcloud compute networks list --project "$P" --format='value(name)'
run gcloud compute routers list --project "$P" --format='value(name,region)'
run gcloud compute firewall-rules list --project "$P" --format='value(name,network)'
run gcloud storage buckets list --project "$P" --format='value(name)'
run gcloud artifacts repositories list --project "$P" --location "$R" --format='value(name)'
run gcloud secrets list --project "$P" --format='value(name)'
run gcloud iam service-accounts list --project "$P" --format='value(displayName)' --filter='email~^podserve'
run gcloud monitoring dashboards list --project "$P" --format='value(displayName)'
n=$(gcloud projects get-iam-policy "$P" --format=json 2>/dev/null |
  jq '[.bindings[].members[] | select(test("podserve"))] | length')
echo "\$ project IAM policy: members mentioning podserve = ${n:-?}"

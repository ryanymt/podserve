#!/usr/bin/env bash
# Spine check (P01 T2), parked as testdata/spine/dashboard/dashboard_check.sh. T9 moves it to
# tests/dashboard.test.sh with `git mv`, unchanged; tests/run.sh then runs it.
#
# It checks dashboards/model-service.json (a Cloud Monitoring dashboard, PromQL over vLLM metrics):
#   1. it is JSON with a displayName, a mosaicLayout of tiles and a model_name dashboard filter;
#   2. every vllm:* metric any query names is one vLLM v0.32.0 exposes
#      (testdata/vllm-v0.32.0-metrics.txt, names as exposed: _total, _bucket, ...);
#   3. each P01 AC6 panel is a tile with this exact title whose queries use these metrics;
#   4. the three latency panels show p50, p95 and p99 (histogram_quantile 0.5 / 0.95 / 0.99);
#   5. every query is filtered by the model_name template variable (contains "${model_name").
# Queries are read from widget.xyChart.dataSets[].timeSeriesQuery.prometheusQuery and
# widget.scorecard.timeSeriesQuery.prometheusQuery.
set -euo pipefail
cd "$(dirname "$0")/.."
dash=dashboards/model-service.json
metrics=testdata/vllm-v0.32.0-metrics.txt
fail=0
err() { echo "dashboard: $*"; fail=1; }

jq -e . "$dash" >/dev/null 2>&1 || { echo "dashboard: $dash is not valid JSON"; exit 1; }
jq -e '.displayName | type == "string" and length > 0' "$dash" >/dev/null || err "no displayName"
# Only fields the Cloud Monitoring Dashboards API accepts: an unknown one passes JSON parsing but is a
# 400 at `terraform apply` (found live in P01 T11: yAxis.units). Check a dashboard against the real
# API with: gcloud monitoring dashboards create --config-from-file=FILE --validate-only
bad=$(jq -r '[.mosaicLayout.tiles[].widget.xyChart? // empty
  | ((.yAxis // {}) | keys[] | select(. != "label" and . != "scale")),
    (.dataSets[]? | keys[] | select(IN("timeSeriesQuery", "plotType", "legendTemplate", "targetAxis", "minAlignmentPeriod", "breakdowns", "dimensions", "measures") | not))]
  | unique | join(" ")' "$dash")
[[ -z $bad ]] || err "fields the Dashboards API rejects: $bad"
# Write the dashboard in the API's normal form, or `terraform plan` shows a change forever: the API
# drops zero-valued fields (xPos/yPos 0) and adds the filter's valueType (found live in P01 T11).
jq -e '[.mosaicLayout.tiles[] | select(.xPos == 0 or .yPos == 0)] | length == 0' "$dash" >/dev/null \
  || err "omit xPos/yPos when they are 0 (the API drops them, so terraform sees a perpetual diff)"
jq -e '[.dashboardFilters[]? | select(.valueType == null)] | length == 0' "$dash" >/dev/null \
  || err "set valueType on every dashboardFilters entry (the API adds \"STRING\")"
jq -e '.mosaicLayout.tiles | type == "array" and length > 0' "$dash" >/dev/null || err "no mosaicLayout.tiles"
jq -e '[.dashboardFilters[]? | select(.templateVariable == "model_name" and .labelKey == "model_name")] | length == 1' "$dash" >/dev/null \
  || err "no dashboardFilters entry with templateVariable and labelKey model_name"

queries_of() { # $1 = jq filter selecting tiles; prints one query per line
  jq -r "[ $1 | .widget | (.xyChart.dataSets[]?.timeSeriesQuery.prometheusQuery?, .scorecard.timeSeriesQuery.prometheusQuery?) ] | map(select(. != null)) | .[]" "$dash"
}

all=$(queries_of '.mosaicLayout.tiles[]')
[[ -n $all ]] || err "no PromQL queries found"
while IFS= read -r q; do
  [[ -z $q ]] && continue
  [[ $q == *'${model_name'* ]] || err "query not filtered by \${model_name}: $q"
  while IFS= read -r m; do
    grep -qx -- "$m" "$metrics" || err "metric $m is not exposed by vLLM v0.32.0 (see $metrics)"
  done < <(grep -oE 'vllm:[a-z0-9_:]+' <<<"$q" | sort -u)
done <<<"$all"

# title | required metrics (space-separated) | needs p50/p95/p99
panels=(
  "Time to first token (p50/p95/p99)|vllm:time_to_first_token_seconds_bucket|yes"
  "Inter-token latency (p50/p95/p99)|vllm:inter_token_latency_seconds_bucket|yes"
  "End-to-end request latency (p50/p95/p99)|vllm:e2e_request_latency_seconds_bucket|yes"
  "Requests per second|vllm:request_success_total|no"
  "Tokens per second (prompt, generation)|vllm:prompt_tokens_total vllm:generation_tokens_total|no"
  "Requests running and waiting|vllm:num_requests_running vllm:num_requests_waiting|no"
  "KV cache usage|vllm:kv_cache_usage_perc|no"
)
for p in "${panels[@]}"; do
  IFS='|' read -r title need quant <<<"$p"
  n=$(jq --arg t "$title" '[.mosaicLayout.tiles[] | select(.widget.title == $t)] | length' "$dash")
  if [[ $n != 1 ]]; then err "want exactly one tile titled \"$title\", found $n"; continue; fi
  qs=$(queries_of ".mosaicLayout.tiles[] | select(.widget.title == \"$title\")")
  for m in $need; do
    grep -q -- "$m" <<<"$qs" || err "\"$title\" does not query $m"
  done
  if [[ $quant == yes ]]; then
    for q in 0.5 0.95 0.99; do
      grep -qE "histogram_quantile\(\s*$q\s*," <<<"$qs" || err "\"$title\" has no histogram_quantile($q, ...)"
    done
  fi
done

(( fail == 0 )) && echo "dashboard: ok ($(wc -l <<<"$all") queries)"
exit $fail

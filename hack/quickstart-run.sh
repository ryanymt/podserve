#!/usr/bin/env bash
# Run the README quickstart word for word, as a new user would (P01 T13; repeat it every phase).
#
#   W=$HOME/qs-run; mkdir -p "$W"
#   env -i HOME="$HOME" PATH="$PATH" USER="$USER" LANG=C.UTF-8 TERM=dumb KUBECONFIG="$W/kubeconfig" \
#     PROJECT=my-gcp-project WORK="$W" [SOURCE=/path/to/repo] [LOADTEST=1] [PAUSE_MIN=0] \
#     bash hack/quickstart-run.sh > "$W/transcript.txt" 2>&1
#
# `env -i` gives the "fresh shell" T13 asks for: an empty KUBECONFIG and nothing inherited. Run it with
# a gcloud default project other than PROJECT, so a missing --project flag shows up.
#
# The commands are the lines of the ```bash blocks in README.md's "## Quickstart" section, taken from
# SOURCE's committed HEAD (the same extraction tests/quickstart.test.sh uses). Each line runs as
# written, with "yes" on stdin for Terraform's prompts. Only these lines are changed or skipped:
#   - `git clone https://github.com/...` clones SOURCE instead, when SOURCE is set (unpublished code);
#   - `export PROJECT=my-gcp-project ...` uses $PROJECT;
#   - the two `gcloud auth ... login` lines are skipped (interactive; sign in beforehand).
# `./podserve port-forward` runs in the background, standing in for the "second terminal".
#
# Extras, each printed as a [run ...] line so the transcript shows what wasn't the quickstart:
#   - LOADTEST=1 (default): before `podserve delete`, run deploy/examples/loadtest-job.yaml so the
#     dashboard has data (AC6).
#   - PAUSE_MIN=N: before `terraform destroy`, wait up to N minutes for "$WORK/continue" to exist
#     (time for a dashboard screenshot).
#   - Afterwards: hack/leftovers.sh.
# The run stops at the first quickstart command that fails, leaving whatever exists. Clean it up, or
# fix the cause and resume with RESUME_FROM="<start of the command line to resume at>" in the same
# WORK: before that line, only the `cd` and `export` lines run (to restore the shell); the rest are
# skipped and say so.
set -u
: "${PROJECT:?set PROJECT}" "${WORK:?set WORK (an empty directory)}"
LOADTEST=${LOADTEST:-1}
PAUSE_MIN=${PAUSE_MIN:-0}
SOURCE=${SOURCE:-}
RESUME_FROM=${RESUME_FROM:-}
here=$(cd "$(dirname "$0")/.." && pwd)
mkdir -p "$WORK/work"

stamp() { date -u +%H:%M:%S; }
note() { echo "[run $(stamp)] $*"; }
PF=""
stop_pf() { if [ -n "$PF" ]; then kill -- -"$PF" 2>/dev/null; PF=""; fi; }
trap stop_pf EXIT

git -C "${SOURCE:-$here}" show HEAD:README.md | awk -v sect="## Quickstart" '
  /^## /      { inside = (index($0, sect) == 1) }
  /^```bash$/ { if (inside) code = 1; next }
  /^```/      { code = 0; next }
  code && $0 !~ /^[[:space:]]*(#|$)/ { print }' > "$WORK/commands.txt"
[ -s "$WORK/commands.txt" ] || { note "no commands found in README.md's ## Quickstart"; exit 2; }

cd "$WORK/work" || exit 2
note "fresh shell: KUBECONFIG=${KUBECONFIG:-<unset>} ($([ -s "${KUBECONFIG:-/nonexistent}" ] && echo non-empty || echo empty)); gcloud default project is $([ "$(gcloud config get project 2>/dev/null)" = "$PROJECT" ] && echo "the target (not fresh)" || echo "another project")"
note "environment: $(env | cut -d= -f1 | sort | tr '\n' ' ')"
note "source: ${SOURCE:-the clone URL in README.md} at $(git -C "${SOURCE:-$here}" rev-parse --short HEAD)"

resuming=0
if [ -n "$RESUME_FROM" ]; then resuming=1; note "resuming at the first line starting with: $RESUME_FROM"; fi
while IFS= read -r line <&3; do
  if (( resuming )); then
    if [[ $line == "$RESUME_FROM"* ]]; then
      resuming=0
    elif [[ $line == "cd "* || $line == "export "* ]]; then
      [[ $line == "export PROJECT="* ]] && line=$(sed -E "s/^export PROJECT=[^ ]+/export PROJECT=$PROJECT/" <<< "$line")
      echo "\$ $line   [$(stamp)]"; eval "$line"; note "re-run to restore the shell"; continue
    else
      note "already done, skipped: $line"; continue
    fi
  fi
  case "$line" in
    "gcloud auth login"|"gcloud auth application-default login")
      echo "\$ $line"; note "skipped: interactive sign-in"; continue ;;
    "git clone https://github.com/"*)
      if [ -n "$SOURCE" ]; then note "clone source replaced: $SOURCE"; line="git clone $SOURCE podserve"; fi ;;
    "export PROJECT="*)
      line=$(sed -E "s/^export PROJECT=[^ ]+/export PROJECT=$PROJECT/" <<< "$line") ;;
  esac

  if [[ $line == "./podserve delete "* ]]; then
    if [ -n "$PF" ]; then stop_pf; note "stopped the port-forward"; fi
    if [ "$LOADTEST" = 1 ]; then
      note "EXTRA (not a quickstart step): load test, so the dashboard has data"
      kubectl apply -f deploy/examples/loadtest-job.yaml 2>&1 | grep -v warnings.go
      kubectl -n podserve wait --for=condition=complete job/loadtest-qwen3-8b --timeout=20m 2>&1 | grep -v warnings.go
      kubectl -n podserve logs job/loadtest-qwen3-8b 2>/dev/null | sed -n '/Serving Benchmark Result/,$p'
      kubectl -n podserve delete job loadtest-qwen3-8b 2>&1 | grep -v warnings.go
      note "EXTRA done"
    fi
  fi

  if [[ $line == "terraform -chdir=deploy/terraform destroy" ]] && [ "$PAUSE_MIN" -gt 0 ]; then
    note "PAUSE (not a quickstart step): waiting up to $PAUSE_MIN min for $WORK/continue"
    for _ in $(seq $((PAUSE_MIN * 6))); do [ -e "$WORK/continue" ] && break; sleep 10; done
    note "resuming ($([ -e "$WORK/continue" ] && echo marker || echo timeout))"
  fi

  echo "\$ $line   [$(stamp)]"
  if [[ $line == "./podserve port-forward "* ]]; then
    note "started in the background, standing in for the second terminal"
    # Its own process group (set -m), so stop_pf reaches kubectl: podserve runs kubectl as a child,
    # and killing only the background subshell left both running.
    set -m; eval "$line" > "$WORK/port-forward.log" 2>&1 < /dev/null & PF=$!; set +m
    # podserve prints its own "Forwarding localhost:N -> NAME" before it starts kubectl, so wait for
    # the port itself to accept connections (as a person typing in a second terminal would).
    port=8000; [[ $line =~ --port[=\ ]([0-9]+) ]] && port=${BASH_REMATCH[1]}
    for _ in $(seq 60); do (exec 9<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null && break; sleep 1; done
    sleep 2; cat "$WORK/port-forward.log"
    # An open port proves nothing if something else holds it: ours must still be running.
    if ! kill -0 "$PF" 2>/dev/null; then
      note "STOPPED: the port-forward exited (port $port held by another process?)"; PF=""; exit 1
    fi
    continue
  fi
  start=$(date +%s)
  { eval "$line"; } <<< "yes" 2>&1
  rc=$?
  echo
  note "exit $rc after $(( $(date +%s) - start ))s"
  if [ $rc -ne 0 ]; then
    note "STOPPED: a quickstart command failed"
    stop_pf
    exit $rc
  fi
done 3< "$WORK/commands.txt"

note "EXTRA (not a quickstart step): leftover check"
bash "$here/hack/leftovers.sh" "$PROJECT"
note "COMPLETE"

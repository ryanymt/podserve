#!/usr/bin/env bash
# Lint everything, offline.
# go vet, gofmt (prints the diff so a worker can fix it with `gofmt -w`), py_compile, terraform fmt
# and validate (providers from the local mirror), and a check that the generated CRD and DeepCopy
# code match pkg/api's markers (regenerated into a temp dir, never in place).
set -euo pipefail
cd "$(dirname "$0")/.."
rc=0

# --- Shell and Python scripts ------------------------------------------------
shopt -s nullglob
files=(tests/*.sh hack/*.sh)
pyfiles=(hack/*.py)
shopt -u nullglob
for f in "${files[@]}"; do bash -n "$f" || { echo "syntax error: $f"; rc=1; }; done
for f in "${pyfiles[@]}"; do python3 -m py_compile "$f" || { echo "python error: $f"; rc=1; }; done
if command -v shellcheck >/dev/null; then
  shellcheck -x -S warning "${files[@]}" || rc=1
else
  echo "lint: shellcheck not installed; ran bash -n only"
fi

# --- Go ------------------------------------------------------------------------
go vet ./... || { echo "lint: go vet failed"; rc=1; }
mapfile -t gofiles < <(git ls-files --cached --others --exclude-standard "*.go")
mapfile -t unformatted < <(gofmt -l "${gofiles[@]}")
if (( ${#unformatted[@]} > 0 )); then
  echo "lint: gofmt would change these files (fix with: gofmt -w <file>):"
  gofmt -d "${unformatted[@]}"
  rc=1
fi
gen=$(mktemp -d)
trap 'rm -rf "$gen"' EXIT
go tool controller-gen crd:crdVersions=v1 paths=./pkg/api/... output:crd:artifacts:config="$gen" \
  && diff -u config/crd/podserve.dev_modelservices.yaml "$gen/podserve.dev_modelservices.yaml" \
  || { echo "lint: config/crd is stale; regenerate it (spine): go tool controller-gen crd ... (see docs/design/modelservice.md)"; rc=1; }
cp pkg/api/v1alpha1/zz_generated.deepcopy.go "$gen/deepcopy.before"
go tool controller-gen object:headerFile=hack/boilerplate.go.txt paths=./pkg/api/... >/dev/null
if ! diff -q "$gen/deepcopy.before" pkg/api/v1alpha1/zz_generated.deepcopy.go >/dev/null; then
  cp "$gen/deepcopy.before" pkg/api/v1alpha1/zz_generated.deepcopy.go   # leave the tree as found
  echo "lint: pkg/api/v1alpha1/zz_generated.deepcopy.go is stale; regenerate it (spine)"
  rc=1
fi

# --- Python (fetch job) --------------------------------------------------------
shopt -s nullglob
for f in images/fetch/*.py; do python3 -m py_compile "$f" || { echo "python error: $f"; rc=1; }; done
shopt -u nullglob
pydirs=()
for d in hack images; do [[ -d $d ]] && pydirs+=("$d"); done
(( ${#pydirs[@]} > 0 )) && find "${pydirs[@]}" -name __pycache__ -type d -prune -exec rm -rf {} + 2>/dev/null || true

# --- Terraform -----------------------------------------------------------------
if [[ -d deploy/terraform && "${SKIP_TERRAFORM_LINT:-0}" != "1" ]]; then
  terraform -chdir=deploy/terraform fmt -check -recursive -diff || { echo "lint: terraform fmt (fix with: terraform fmt -recursive deploy/terraform)"; rc=1; }
  terraform -chdir=deploy/terraform init -backend=false -plugin-dir="$HOME/.cache/podserve/tf-mirror" \
    -input=false -no-color >/dev/null || { echo "lint: terraform init (offline) failed"; rc=1; }
  terraform -chdir=deploy/terraform validate -no-color || rc=1
fi

(( rc == 0 )) && echo "lint: ok"
exit $rc

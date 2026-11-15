# Contributing to podserve

Thank you for your interest in contributing to **podserve**!

If you have questions or want to discuss a larger change before opening a pull request, reach out at **dev@podserve.dev** or open an issue on GitHub.

## Prerequisites

- Go `>= 1.26`
- Python `>= 3.11` with `pyyaml`
- Terraform `>= 1.14`
- `jq`, `git`, and optionally `shellcheck`

## Running tests and lint offline

Before submitting changes, verify that the build, unit tests, and linters pass locally:

```bash
# One-time setup: mirror Terraform providers for offline validation/testing
terraform -chdir=deploy/terraform providers mirror "$HOME/.cache/podserve/tf-mirror"

go build ./...
bash tests/run.sh
bash tests/lint.sh
```

### Regenerating generated files

If you modify API types in `pkg/api/v1alpha1/types.go`, regenerate the CRD and DeepCopy code (and ensure `docs/design/modelservice.md` stays in sync via `go run ./hack/specfields`):

```bash
go tool controller-gen object:headerFile=hack/boilerplate.go.txt paths=./pkg/api/...
go tool controller-gen crd:crdVersions=v1 paths=./pkg/api/... output:crd:artifacts:config=config/crd
```

If you modify the GPU sizing logic in `pkg/profile` or `catalog/gpus.yaml`, keep `testdata/spine/profile/expected.py`, `docs/design/profile.md`, and `testdata/profile/expected.json` aligned (`tests/profile-expected.test.sh` enforces this).

If you change quickstart commands in `README.md`, keep `docs/quickstart.md` aligned (`tests/quickstart.test.sh` verifies that every command in `README.md`'s `## Quickstart` section appears in order in `docs/quickstart.md`).

## License

By contributing to podserve, you agree that your contributions will be licensed under the [Apache License, Version 2.0](LICENSE).

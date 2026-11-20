# Platform contract

Core document (SYSTEM.md). It fixes the names, layouts and interfaces that tie together Terraform
(`deploy/terraform/`), the CLI (`cmd/podserve`), the fetch job (`images/fetch/`) and the serving pods.
`pkg/platform/platform.go` is its code; the two must agree.

## Names (fixed, D-024)

| What | Value |
|---|---|
| Namespace | `podserve` |
| Settings ConfigMap | `podserve/podserve-platform` |
| Fetch identity (KSA) | `podserve/podserve-fetch` |
| Serving identity (KSA) | `podserve/podserve-model` |
| Server-side apply field manager | `podserve` |
| Serving port | `8000` (vLLM's OpenAI server) |
| GCS completion marker | `_PODSERVE_COMPLETE` |

All of podserve's objects live in the `podserve` namespace. `deploy` refuses any other. Terraform's
Workload Identity grants name these service accounts, so they are constants, not settings.

**The model bucket's name** is `<NAME>-models-<HASH>`: the platform name (`podserve` by default),
then the first 12 hex characters of `sha256(<PROJECT_ID>)` (D-031). vLLM shows its `--model` path,
`gs://<BUCKET>/…`, as `root` in `/v1/models`, so the name must not reveal the project ID. The hash
is fixed per project, so a re-apply keeps the bucket. For `my-project` it is
`podserve-models-1c7cd944c28c`.

## The `podserve-platform` ConfigMap

Written by `podserve init`. Every key is required.

| Key | Example | Source |
|---|---|---|
| `project` | `my-project` | Terraform output `project_id` |
| `region` | `us-central1` | Terraform output `region` |
| `bucket` | `podserve-models-1c7cd944c28c` | Terraform output `model_bucket` (below) |
| `vllmImage` | `us-central1-docker.pkg.dev/my-project/podserve-dockerhub/vllm/vllm-openai@sha256:befccf0e…` | Terraform output `dockerhub_mirror` + the digest pinned in `pkg/platform` |
| `fetchImage` | `us-central1-docker.pkg.dev/my-project/podserve/fetch@sha256:…` | built by `podserve init` (D-023), or `--fetch-image` |
| `hfTokenSecret` | `projects/my-project/secrets/podserve-hf-token` | Terraform output `hf_token_secret` (no version) |

The pinned vLLM image is `vllm/vllm-openai:v0.32.0` = `sha256:befccf0e7e4089aa23926bb6f887a85c0d46c2a99e6de5c5ffdf5cb7957c5b34`
(the multi-arch index digest). It needs NVIDIA driver 580 or later (CUDA 13.0, compatibility mode off).

## Terraform outputs consumed by `podserve init --config`

`podserve init --config FILE` reads `terraform output -json` and needs these outputs, each with a
`value`: `project_id`, `region`, `model_bucket`, `dockerhub_mirror` (the remote repo's path,
`REGION-docker.pkg.dev/PROJECT/REPO`), `images_repo` (the standard repo's path), `hf_token_secret`,
`build_service_account` (email), `cluster_name`.

## IAM (D-013, D-023)

Granted by Terraform to Workload Identity Federation principals, with no Google service accounts for
workloads:

| Principal | Roles | Scope |
|---|---|---|
| `…/subject/ns/podserve/sa/podserve-fetch` | `roles/storage.objectAdmin`, `roles/storage.legacyBucketReader` | model bucket |
| `…/subject/ns/podserve/sa/podserve-fetch` | `roles/secretmanager.secretAccessor` | the HF-token secret |
| `…/subject/ns/podserve/sa/podserve-model` | `roles/storage.objectViewer`, `roles/storage.legacyBucketReader` | model bucket |

`roles/storage.legacyBucketReader` is required. Run:ai Model Streamer's GCS client calls
`get_bucket()` before listing files, and that needs `storage.buckets.get`, which no object role includes.
Without it vLLM crash-loops with a 403 (discovered during live cluster testing).

Two Google service accounts exist for infrastructure only:
- **The node service account:** `roles/container.defaultNodeServiceAccount`, plus
  `roles/artifactregistry.reader` on the two repositories.
- **The Cloud Build service account** (D-023): `roles/artifactregistry.writer` on the images repo,
  `roles/logging.logWriter`, and `roles/storage.objectAdmin` and `roles/storage.legacyBucketReader`
  on the model bucket for the staging prefix `_build/`. `gcloud builds submit` needs
  `storage.buckets.get` on the staging bucket, which no object role grants (found live in T11).

## GCS layout

```
gs://BUCKET/
  models/hf/ORG/MODEL/SHA/            one pinned revision (vLLM's --model is this, with a trailing /)
    config.json, *.safetensors, tokenizer files, ...
    _PODSERVE_COMPLETE                written last by the fetch job
  _build/                             Cloud Build source staging (D-023)
```

`ModelURI(bucket, repo, sha)` = `gs://BUCKET/models/hf/ORG/MODEL/SHA/`.

## Labels

Every object podserve creates carries `app.kubernetes.io/managed-by: podserve`. The others are listed
below.

| Label | On | Value |
|---|---|---|
| `podserve.dev/model` | ModelService children, their pods | the ModelService name |
| `podserve.dev/revision` | ModelService children, fetch Job | the commit SHA |
| `app.kubernetes.io/component` | Deployment and its pods: `server`; fetch Job and its pod: `fetch` | |
| `podserve.dev/fetch` | fetch Job and its pod | `FetchHash(repo, sha)` |

## The fetch Job interface (T6 renders it, T7 implements it)

**Name:** `podserve-fetch-HASH`, where HASH = the first 12 hex characters of
sha256("ORG/MODEL@SHA"). The Job has the annotation `podserve.dev/repo: ORG/MODEL`, the label
`podserve.dev/revision: SHA`, and the rendered shape in `testdata/render/*.golden.yaml`.

**Shape:**
- **Service account:** `podserve-fetch`.
- **Ownership:** no owner reference. `ttlSecondsAfterFinished: 3600`, `backoffLimit: 2`.
- **Command:** `python -I /app/fetch.py`. Isolated mode keeps the script's directory off `sys.path`.
- **Scratch volume:** a generic ephemeral volume mounted at `/scratch`, sized at the largest file plus
  2 GiB, never below 10 GiB, storage class `standard-rwo`.

**Environment variables** (the only inputs):

| Variable | Example |
|---|---|
| `PODSERVE_REPO` | `Qwen/Qwen3-8B` |
| `PODSERVE_REVISION` | the 40-character commit SHA |
| `PODSERVE_DEST` | `gs://BUCKET/models/hf/Qwen/Qwen3-8B/SHA` (no trailing slash) |
| `PODSERVE_HF_TOKEN_SECRET` | `projects/P/secrets/S` |
| `PODSERVE_SCRATCH` | `/scratch` |
| `HF_HUB_DISABLE_PROGRESS_BARS` | `1` |

**Behaviour:**
1. If `DEST/_PODSERVE_COMPLETE` exists, print `{"event":"cached"}` and exit 0.
2. Read the latest version of the secret. If the secret has **no version**, or the version is empty,
   use no token. Any other Secret Manager error is fatal (exit 1).
3. List the revision's files. If the model is gated and there's no token, print
   `{"event":"error","code":"GATED_NO_TOKEN"}` and exit 3.
4. Select files with `FetchFileAllowed`: top-level files ending in `.safetensors`, `.json`, `.txt`,
   `.model`, `.jinja` or `.tiktoken`. With no `.safetensors`, print
   `{"event":"error","code":"NO_SAFETENSORS"}` and exit 4.
5. For each file, one at a time: download into `/scratch`, upload to `DEST/NAME`, check that the
   uploaded size equals the Hub's size, delete the local copy, and print
   `{"event":"file","name":…,"bytes":…,"seconds":…}`.
6. Write `DEST/_PODSERVE_COMPLETE` last, and print `{"event":"done","files":N,"bytes":B,"seconds":S}`.

The marker is JSON: `{"repo":…,"revision":…,"files":[{"name":…,"size":…}],"podserve":"VERSION"}`.

**Exit codes:** 0 ok or cached · 1 unexpected error · 3 `GATED_NO_TOKEN` · 4 `NO_SAFETENSORS` ·
5 size mismatch. Every progress line on stdout is one JSON object.

**Termination log.** Before exiting, `fetch.py` writes its final event as one JSON line to
`/dev/termination-log`, Kubernetes' default `terminationMessagePath`:
- `{"event":"cached"}` when the marker already existed
- the `done` event after a copy
- the `error` event on failure (for unexpected errors, `{"event":"error","code":"UNEXPECTED","message":…}`)

Kubernetes copies it into the pod's `status.containerStatuses[].state.terminated.message`. `pkg/kube`
reads it there, because the dynamic client can't read logs:
- `cached` gives `FetchCached`; any other success gives `FetchCopied`.
- On failure, the error's `code` becomes `FetchError.Code`, or `UNKNOWN` if there's no message.

**Python seam** (`images/fetch/fetch.py`; the tests drive it with fakes):
```
run(env, hub, store, secrets, out, termination_log) -> exit code
hub.model_info(repo, revision, token)               -> (gated: bool, files: [(name, size), ...]) in Hub order
hub.download(repo, revision, name, dest_dir, token) -> local path of the downloaded file
store.exists(bucket, object_name) -> bool
store.upload(bucket, object_name, local_path) -> size of the uploaded object
store.write_text(bucket, object_name, text)
secrets.latest(secret_name) -> str, or None when the secret has no (enabled) version
```
- **Tokens:** a whitespace-only secret value counts as no token. The token is stripped.
- **Order:** files are processed in Hub order.
- **`main()`** wraps the real clients (huggingface_hub, google-cloud-storage,
  google-cloud-secret-manager) behind these methods and calls `run(os.environ, ...)`.

**Dependencies:**
- Runtime, in `images/fetch/requirements.txt`, pinned: `huggingface_hub`, `hf_xet`,
  `google-cloud-storage`, `google-cloud-secret-manager`.
- Test only, not in the image: `PyYAML`. `test_fetch.py` reads T6's fetch-Job golden file with it.

## Token sources

| Command | Token |
|---|---|
| `podserve plan` | `HF_TOKEN` from the local environment (needed to read a gated model's config) |
| `podserve deploy` | doesn't need a local token. The fetch Job reads Secret Manager. |

A deploy of a gated model with no secret version fails in the fetch Job with `GATED_NO_TOKEN`, and the
CLI shows that message.

## Data files and how they're regenerated

- **Architectures:** `catalog/vllm-v0.32.0-architectures.txt` and **exposed metric names:**
  `testdata/vllm-v0.32.0-metrics.txt`. Both are regenerated with:
  ```
  hack/vllm-source.sh v0.32.0 /tmp/vllm-src
  python3 -I hack/vllm-archs.py   /tmp/vllm-src/registry.py > catalog/vllm-v0.32.0-architectures.txt
  python3 -I hack/vllm-metrics.py /tmp/vllm-src/loggers.py  > testdata/vllm-v0.32.0-metrics.txt
  ```
- Both scripts parse the files with `ast`, never executing them. The metric names are the ones
  Prometheus *exposes* (`_total`, `_bucket`, `_sum`, `_count`), which is what the dashboard queries.

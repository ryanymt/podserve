# The `podserve` CLI

Core document (SYSTEM.md). It defines every command, flag, output line and exit code. T10b implements
it, and `testdata/cli/*.golden` pins it. Placeholders are in `<ANGLE>` brackets; everything else is
literal. Normal output goes to stdout; errors go to stderr, prefixed `podserve: `.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | unexpected error (a bug, or an unclassified API error) |
| 2 | usage: bad arguments or flags, or an invalid spec |
| 3 | refused: the settings engine won't serve this model here (any refusal code, or `NOT_DEPLOYABLE`) |
| 4 | not found or unreachable: no such model on the Hub, no such ModelService, platform not initialised, cluster unreachable |
| 5 | timed out waiting |
| 6 | the fetch Job failed (`GATED_NO_TOKEN`, `NO_SAFETENSORS`, size mismatch, other) |

## Model sources

`SOURCE` is `hf://ORG/MODEL` or `hf://ORG/MODEL@REVISION`. `--revision` is the alternative to `@`.
Giving both is a usage error (exit 2):
`podserve: give the revision either as @REVISION or with --revision, not both`.
Any other `SOURCE` is also a usage error: `podserve: model source <SOURCE>: want hf://ORG/MODEL`.
The default revision is `main`. Each command resolves the revision to
a commit SHA and prints its first 12 characters.

## `podserve version`

Prints `podserve <VERSION>` and exits 0.

## `podserve plan SOURCE [flags]`

Needs no cluster. Uses `HF_TOKEN` from the environment if it's set.

| Flag | Default | Spec field |
|---|---|---|
| `--revision REV` | `main` | `spec.model.revision` |
| `--gpu TYPE` | `auto` | `spec.accelerator.type` |
| `--gpus N` | `0` (choose) | `spec.accelerator.count` |
| `--max-model-len N` | `0` (choose) | `spec.engine.maxModelLen` |
| `--gpu-memory-percent P` | `90` | `spec.engine.gpuMemoryPercent` |
| `-o, --output text\|json` | `text` | |

Text output for a profile. Labels are padded to 14 columns, sizes are GiB to 1 decimal place, and
integers have no separators:
```
Model:        Qwen/Qwen3-8B @ b968826d9c46 (text-generation, apache-2.0)
Architecture: Qwen3ForCausalLM (supported by vLLM v0.32.0)
Weights:      15.3 GiB, served as bfloat16
GPU:          1x nvidia-l4 on g2-standard-8 (6 CPU, 24Gi)
Parallelism:  tensor-parallel 1
Context:      17408 tokens (model maximum 40960)
KV cache:     2.5 GiB, 18110 tokens, 1 sequence(s) at full context
Deployable:   yes
```
- **No pipeline tag:** the Model line shows `(no pipeline tag, <LICENSE>)`.
- **Missing licence:** shown as `unknown licence`.
- **Not deployable:** the last line is
  `Deployable:   no (podserve v0.1 deploys nvidia-l4 only)`. Exit 0, since `plan` succeeded.
- **Warnings:** each `Result.Warnings` entry follows as `warning: <TEXT>`.

JSON output is one object, indented by two spaces:
- for a profile: `{"model": {...}, "profile": {...}}`
- for a refusal: `{"model": {...}, "refusal": {"code": ..., "message": ...}}`

`model` holds `repo`, `revision` (full SHA), `pipelineTag`, `license` and `gated`. `profile` is
`profile.Result` with its JSON tags. With `-o json`, a refusal also exits 3.

### Refusals

The error line on stderr, exit 3:
```
podserve: cannot serve <ORG/MODEL>: <MESSAGE> (<CODE>)
```

`<MESSAGE>` is `profile.Refusal.Message`, exactly as in this table:

| Code | Message |
|---|---|
| `NOT_TEXT_GENERATION` | `its Hub pipeline tag is <PIPELINE_TAG>; podserve v0.1 serves text-generation models only` |
| `GATED_NO_TOKEN` | `the model is gated; set HF_TOKEN to a token whose owner has accepted its licence` |
| `INVALID_CONFIG` | `config.json is unreadable or incomplete: <DETAIL>` |
| `UNSUPPORTED_ARCHITECTURE` | `architecture <ARCH> is not supported by vLLM v0.32.0` |
| `NO_SAFETENSORS` | `the repository has no .safetensors weights; podserve does not load pickle (.bin) weights` |
| `UNKNOWN_GPU_TYPE` | `unknown GPU type <TYPE>; known types: <T1>, <T2>, ...` (catalogue order) |
| `DOES_NOT_FIT` | `needs about <W> GiB for weights; the largest option tried (<N>x <TYPE>) has room for <R> GiB`, or, when no shape suits the attention heads, `needs about <W> GiB for weights; no catalogued GPU shape suits its <H> attention heads` |
| `NOT_DEPLOYABLE` | (CLI only) `it needs <N>x <TYPE>, which podserve v0.1 does not deploy (v0.1 deploys nvidia-l4 only)` |

`<DETAIL>` for `INVALID_CONFIG`:

| Case | Detail |
|---|---|
| no config | `config.json could not be read` |
| empty architectures | `no architectures` |
| missing field | `missing <FIELD>`, e.g. `missing num_hidden_layers` |
| index names an absent file | `index lists <FILE>, which is not in the repository` |
| requested context too long | `maxModelLen <N> exceeds the model's maximum <M>` |

### Hub errors

| Case | stderr | Exit |
|---|---|---|
| Not found | `podserve: <ORG/MODEL>@<REV> not found on the Hugging Face Hub` | 4 |
| Access denied | `podserve: the Hugging Face Hub refused access to <ORG/MODEL> with this token; accept the model's licence on huggingface.co` | 3 |

## `podserve init --config FILE [--fetch-image REF]`

- **Reads** `FILE`, which is `terraform output -json`. The required outputs are listed in
  `platform.md`. A missing output prints `podserve: <FILE> has no output "<NAME>"` and exits 2.
- **Builds the fetch image** (D-023), unless `--fetch-image` is given. It runs from the repository root
  (`images/fetch/Dockerfile` must exist); otherwise it prints
  `podserve: images/fetch not found; run init from the podserve repository root or pass --fetch-image`
  and exits 2. Two commands are exec'd:
  ```
  gcloud builds submit images/fetch --project <PROJECT> --region <REGION> \
    --tag <IMAGES_REPO>/fetch:<VERSION> \
    --service-account projects/<PROJECT>/serviceAccounts/<BUILD_SA> \
    --gcs-source-staging-dir gs://<BUCKET>/_build/source --gcs-log-dir gs://<BUCKET>/_build/logs
  gcloud artifacts docker images describe <IMAGES_REPO>/fetch:<VERSION> --project <PROJECT> \
    --format value(image_summary.digest)
  ```
  The second prints the digest. The image reference is `<IMAGES_REPO>/fetch@<DIGEST>`.
- **Applies** with server-side apply: the CRD (embedded), the namespace `podserve`, the service
  accounts `podserve-fetch` and `podserve-model`, and the ConfigMap `podserve/podserve-platform`.
  `vllmImage` = `<DOCKERHUB_MIRROR>/vllm/vllm-openai@<VLLM_DIGEST>`.
- **Prints:**
  ```
  Building the fetch image with Cloud Build...
  Fetch image: <IMAGE_REF>
  Installed CRD modelservices.podserve.dev
  Created namespace podserve and service accounts podserve-fetch, podserve-model
  Saved platform settings to podserve/podserve-platform
  podserve is ready. Next: podserve deploy hf://Qwen/Qwen3-8B
  ```
  With `--fetch-image`, the first two lines are replaced by `Fetch image: <REF> (given)`.

## `podserve deploy (SOURCE [flags] | -f FILE) [--dry-run] [--timeout D]`

**Flags.** It takes `plan`'s flags (not `-o`), plus:

| Flag | Default | Spec field |
|---|---|---|
| `--name NAME` | derived (below) | `metadata.name` |
| `--served-model-name N` | the repo id | `spec.servedModelName` |
| `--spot BOOL` | `true` | `spec.accelerator.spot` |
| `--replicas N` | `1` | `spec.replicas` |
| `--timeout D` | `30m` | |
| `--dry-run` | | |

- **Derived name:** the model part of the repo, lowercased. Characters outside `[a-z0-9-]` become `-`,
  repeated `-` collapse, and the result is trimmed of `-` and cut to 40 characters. If it doesn't
  start with a letter, it gets the prefix `m-`. So `Qwen/Qwen3-8B` becomes `qwen3-8b`.
- **`-f FILE`** loads a ModelService (YAML). Combining `-f` with `SOURCE` or with spec flags is a
  usage error.

**Order.** Parse, default and validate the spec, then read the platform ConfigMap, then plan, then
(unless `--dry-run`) touch the cluster.

**Validation.** Defaulted, then validated (`modelservice.md`). Each problem prints
`podserve: invalid spec: <PATH>: <MESSAGE>`, then the command exits 2.

**Planning.** Done exactly as in `plan`, with the local `HF_TOKEN`. Planning reads `config.json`, so
a gated model needs a token in two places: locally, to plan, and in the platform's secret, for the
fetch Job. Without the local one, deploy refuses with `GATED_NO_TOKEN`. A refusal, or
`NOT_DEPLOYABLE`, exits 3, with the `plan` error line and no `Planned:` line. A profile prints:
```
Planned: <N>x <TYPE>, tensor-parallel <N>, context <LEN> (<ORG/MODEL> @ <SHA12>)
```

**`--dry-run`** prints the rendered objects as YAML documents separated by `---`: the ModelService,
the fetch Job, the Deployment, the Service, the PodMonitoring. Owner references carry uid
`00000000-0000-0000-0000-000000000000`. It writes nothing to the cluster, but it reads the platform
ConfigMap. Exit 0.

**Steps.** Each stage line is printed once, as it's reached, with elapsed time since `deploy` started
as `m:ss`. The stage name is padded to 10 columns. The CLI prints the `record` and `fetch` lines
itself, around its calls to `pkg/kube`. The `node`, `image`, `loading` and `ready` lines come from
`kube.Options.Progress` during `WaitReady`, which reports each stage the first time it sees it, and
always `ready` on success. So a deployment that's ready at the first check prints only the `ready`
line. The CLI passes its own clock (`deps.Now` and `deps.Sleep`) to `kube.Options.Clock`.
```
[0:01] record    applied podserve/<NAME>
[0:01] fetch     caching <ORG/MODEL>@<SHA12> into gs://<BUCKET>/models/hf/<ORG/MODEL>/<SHA12>...
[0:06] fetch     done
[0:06] node      waiting for a GPU node (<N>x <TYPE>, spot)
[2:51] image     pulling the vLLM image
[2:54] loading   loading weights and warming up
[7:21] ready     <NAME> is ready
Endpoint (in cluster): http://<NAME>.podserve.svc:8000/v1
Try it: podserve port-forward <NAME>   then   curl localhost:8000/v1/models
```
- **Cached weights:** when the fetch Job is already complete, or finds the marker, the second fetch
  line reads `fetch     cached` and has no `done` line.
- **On-demand:** the node line says `on-demand` instead of `spot`.
- **Status:** written through `/status`: `Caching` during fetch, `Deploying` after, `Ready` at the
  end, `Failed` with a message on failure.

**Failures:**

| Case | stderr | Exit |
|---|---|---|
| Fetch Job failed | `podserve: fetching <ORG/MODEL> failed: <CODE>` plus a hint for `GATED_NO_TOKEN`: `add a token with: gcloud secrets versions add <SECRET_NAME> --data-file=-` | 6 |
| Timeout | `podserve: <NAME> not ready after <D> (stage: <STAGE>); see: kubectl -n podserve logs deploy/<NAME>` | 5 |
| Platform not initialised | `podserve: platform not initialised: ConfigMap podserve/podserve-platform not found; run podserve init` | 4 |

## `podserve list [-o text|json]`

```
NAME       MODEL                REVISION      GPU            PHASE   ENDPOINT
qwen3-8b   hf://Qwen/Qwen3-8B   b968826d9c46  1x nvidia-l4   Ready   http://qwen3-8b.podserve.svc:8000/v1
```
- **Layout:** written with `tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)`, so each column is as wide as
  its longest cell plus two spaces, and the last column isn't padded. Rows are sorted by name. A
  missing status value prints `-`. `GPU` is `<COUNT>x <TYPE>` from the status profile, `REVISION`
  the first 12 characters of `status.resolvedRevision`, and `MODEL` the spec's source.
- **Empty:** `No models deployed.`
- **JSON:** an array of `{"name", "source", "revision", "gpuType", "gpuCount", "phase", "endpoint"}`.

## `podserve delete NAME [--timeout D]`

Default timeout `10m`. It deletes the ModelService, waits until its pods are gone, and removes the
revision's finished fetch Job when no other ModelService uses that revision. It prints
`Deleted <NAME> (pods gone after <m:ss>)`.

| Case | stderr | Exit |
|---|---|---|
| Unknown name | `podserve: no model named <NAME>` | 4 |
| Timeout | `podserve: <NAME>'s pods still running after <D>` | 5 |

## `podserve port-forward NAME [--port N]`

Default port `8000`. It checks that the ModelService exists (exit 4 as for `delete`), prints
`Forwarding localhost:<N> -> <NAME> (Ctrl-C to stop)`, then execs
`kubectl port-forward -n podserve svc/<NAME> <N>:8000`, and exits with kubectl's exit code (D-018).

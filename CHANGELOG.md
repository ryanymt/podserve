# Changelog

## v0.1.1

A hardening release before the v0.2 operator. `podserve deploy` now says why a model won't start,
instead of waiting 30 minutes.

### Changed

- **`deploy` fails fast, with the reason.**
  - It stops as soon as Kubernetes reports that the model can't start:
    - a crash loop;
    - an image that can't be pulled for 2 minutes;
    - an invalid image name;
    - a container that can't be created.
  - It prints the reason in Kubernetes' own words. For a crash loop it also prints the container's
    exit code and the last lines of vLLM's log, for example
    `vllm: error: unrecognized arguments: --no-such-flag`.
  - It exits 7, a new exit code: "the model failed to start".
- **A failed or timed-out deploy releases the GPU.** The model is scaled to zero, so Autopilot
  releases the GPU node, and its record stays with phase `Failed` and the reason. Run
  `podserve deploy` again to retry, or `podserve delete` to remove it. Before, a crash-looping pod
  kept its GPU node until you deleted the model.
- **`deploy` says why it's waiting.** A line prints under the current stage each time the reason
  changes:
  - `TriggeredScaleUp`: Autopilot is adding a node;
  - `FailedScaleUp` or `NotTriggerScaleUp`: Autopilot couldn't or wouldn't;
  - `TerminationByKubelet`: a spot node was reclaimed and the pod is being replaced;
  - `ErrImagePull`: an image pull is failing, for under 2 minutes so far.

  If the pod goes back to an earlier stage, for example to `node` after its spot node is
  reclaimed, that stage's line prints again. A timeout names the last reason, not just the stage.
- **Readiness counts only the newest pods.** Redeploying a model over a running one no longer
  reports ready from the old pod, and old pods that are still shutting down no longer fail the new
  deploy.
- **The model bucket's name no longer contains your project ID.** vLLM shows the `gs://` path of the
  weights in `/v1/models`, which every API client can read. The bucket is now
  `podserve-models-<12 hex characters of a hash of the project ID>`. See "Upgrading" below.
- **`extraArgs` can't set an argument podserve owns, in any form vLLM accepts.** That covers
  underscores (`--max_model_len`), abbreviations (`--max-model`, `-t`), dotted keys
  (`--model-loader-extra-config.x`), one item with a space (`"--port 8001"`), and `--config FILE`.
- **The Go module is `github.com/ryanymt/podserve`.** You can install the CLI with
  `go install github.com/ryanymt/podserve/cmd/podserve@v0.1.1`, and `podserve version` prints the
  version it was built from.

### Added

- **`podserve status NAME [-o text|json]`.** It shows the model's phase, the stage its pods are at,
  and why it is waiting or failed.

### Fixed

- **`deploy`'s fetch line** printed a GCS path that didn't exist (a 12-character commit ID). It now
  prints the real path, with the full commit SHA.
- **Hub requests for a revision that contains `/`** (such as `refs/pr/1`) went to the wrong URL.
- **`podserve init`** printed Cloud Build's whole log on stdout. The log now goes to stderr.
- **`deploy`** no longer prints Autopilot's routine admission warnings ("Autopilot added
  tolerations…"). Other warnings still print.

### Upgrading from v0.1

The model bucket is renamed, so an existing install moves to a new, empty bucket. Its cached weights
are copied from the Hub once more on the next deploy of each model.
1. `./podserve delete <NAME>` for each model (`./podserve list` shows them).
2. `terraform -chdir=deploy/terraform apply`. It replaces the model bucket.
3. `terraform -chdir=deploy/terraform output -json > platform.json`, then
   `./podserve init --config platform.json`. The platform settings then point at the new bucket.
4. `./podserve deploy …` each model again.

Skipping step 1 leaves running models pointed at the old bucket. A fetch Job less than an hour old
would also be reused against it.

## v0.1

The first release: one command takes a Hugging Face text model to an OpenAI-compatible endpoint on
a GKE Autopilot spot L4.
- `terraform apply` and `podserve init` set up the platform.
- `podserve plan` sizes a model without a GPU.
- `podserve deploy` caches the weights in your GCS bucket and serves them with vLLM.
- A Cloud Monitoring dashboard shows each model.

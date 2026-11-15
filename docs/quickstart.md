# Quickstart: serve a Hugging Face model on GKE

This is the README's quickstart with each step explained. The commands are the same and in the same
order. A few optional extras are marked as such. Times are from a real benchmark run in `us-central1`.

**What you'll have at the end:** Qwen3-8B on a spot NVIDIA L4 in a private GKE Autopilot cluster in
your project, answering OpenAI-compatible requests. Then you'll remove all of it.

## Before you start

- **A GCP project with billing linked.** Terraform enables the APIs it needs.
- **GPU quota.**
  - Spot ("preemptible") NVIDIA L4 GPUs in your region: at least 1 for Qwen3-8B, 2 for Qwen3-14B.
  - Check it in the console under *IAM & Admin → Quotas*, searching for "Preemptible NVIDIA L4 GPUs".
- **Tools:**
  - [gcloud](https://cloud.google.com/sdk/docs/install).
  - Terraform ≥ 1.14.
  - Go ≥ 1.26, to build the CLI.
  - `kubectl` and `gke-gcloud-auth-plugin`: `gcloud components install kubectl
    gke-gcloud-auth-plugin`, or your package manager.
- **Organisation policies.** podserve creates no service-account keys and gives nodes no external
  IPs. It has been tested in one project whose organisation policies were relaxed, so a stricter
  organisation may block a step. If `terraform apply` fails on a constraint, please open an issue
  with the constraint's name.

## 1. Sign in

```bash
gcloud auth login
gcloud auth application-default login
```

- `gcloud auth login` is for gcloud itself and for `podserve`, which uses gcloud's credentials
  through your kubeconfig.
- Application-default credentials are what Terraform uses.

## 2. Get podserve and build the CLI

```bash
git clone https://github.com/ryanymt/podserve.git
cd podserve
go build -o podserve ./cmd/podserve
```

Run every later command from this directory. `podserve init` builds the fetch image from
`images/fetch/` here.

## 3. Choose your project and region

```bash
export PROJECT=my-gcp-project REGION=us-central1
```

Every command below names the project explicitly, so your gcloud default project doesn't matter.

## 4. A bucket for Terraform's state

```bash
gcloud storage buckets create gs://$PROJECT-podserve-tfstate --project $PROJECT --location $REGION --uniform-bucket-level-access --public-access-prevention
gcloud storage buckets update gs://$PROJECT-podserve-tfstate --versioning
```

- Terraform keeps its state here rather than on your laptop. Anyone on your team can run the next
  apply or destroy.
- Versioning keeps old copies of the state.
- `terraform destroy` doesn't remove this bucket. Delete it yourself when you're done for good
  (see [Clean up](#8-clean-up)).

## 5. Create the platform

```bash
printf 'project_id = "%s"\nregion     = "%s"\n' $PROJECT $REGION > deploy/terraform/terraform.tfvars
terraform -chdir=deploy/terraform init -backend-config="bucket=$PROJECT-podserve-tfstate"
terraform -chdir=deploy/terraform apply
terraform -chdir=deploy/terraform output -json > platform.json
```

`apply` shows its plan and asks for `yes`. It takes about **10 minutes**; the cluster is 8 of them.
It creates:
- **Network:** a VPC and subnet with Cloud NAT and Private Google Access. Nodes have no public IPs.
- **Cluster:** a regional GKE Autopilot cluster with private nodes and Workload Identity. Its
  control plane is reached through its DNS endpoint.
- **Storage:** a Cloud Storage bucket for model weights.
- **Artifact Registry:** a Docker Hub mirror (the vLLM image comes through it) and a repository
  for podserve's own images.
- **Secret Manager:** a secret for a Hugging Face token. It is empty; you need it only for gated
  models.
- **Identity:** permissions for the two workloads, as Workload Identity principals, so there are no
  service-account keys. Plus a node service account and a Cloud Build service account.
- **Monitoring:** the Cloud Monitoring dashboard "podserve: model service".

It creates **no GPU capacity.** Autopilot adds a GPU machine when a model's pod asks for one, and
removes it when the pod goes.

`platform.json` holds the outputs that `podserve init` reads: bucket and repository names, and the
like. It holds no secrets.

## 6. Connect and prepare the cluster

```bash
gcloud container clusters get-credentials podserve --region $REGION --project $PROJECT --dns-endpoint
./podserve init --config platform.json
```

- `get-credentials` writes a kubeconfig entry for the cluster's DNS endpoint and makes it the
  current context.
- `podserve init` takes about **45 seconds**. It builds the fetch image with Cloud Build into your
  registry. Then it installs the `ModelService` CRD, the `podserve` namespace, two Kubernetes
  service accounts, and a ConfigMap with the platform settings.

It ends with:

```text
podserve is ready. Next: podserve deploy hf://Qwen/Qwen3-8B
```

## 7. Serve a model

```bash
./podserve plan hf://Qwen/Qwen3-8B
./podserve deploy hf://Qwen/Qwen3-8B
```

`plan` reads the model's `config.json` from the Hub and shows what `deploy` would do. It needs no
GPU and doesn't touch the cluster:

```text
Model:        Qwen/Qwen3-8B @ b968826d9c46 (text-generation, apache-2.0)
Architecture: Qwen3ForCausalLM (supported by vLLM v0.32.0)
Weights:      15.3 GiB, served as bfloat16
GPU:          1x nvidia-l4 on g2-standard-8 (6 CPU, 24Gi)
Parallelism:  tensor-parallel 1
Context:      17408 tokens (model maximum 40960)
KV cache:     2.5 GiB, 18114 tokens, 1 sequence(s) at full context
Deployable:   yes
```

`deploy` prints each stage as it happens. Measured, the first time:

| Stage | Took | What's happening |
|---|---|---|
| `fetch` | 6:47 | A Job copies 16 GB of safetensors from the Hub into your bucket, pinned to the commit |
| `node` | 2:28 | Autopilot provisions a spot g2-standard-8 with one L4 |
| `image` | 2:01 | The node pulls vLLM through your Docker Hub mirror. It took 5 s once the mirror was warm |
| `loading` | 4:29 | vLLM streams the weights from GCS, profiles memory, and warms up |
| **Ready** | **15:48** | |

A second deploy of the same model and revision skips the copy: the cached run took 7:12.

You may see lines like `Warning: autopilot-workload-defaulter:Autopilot added tolerations…` on
stderr. They're Autopilot telling you what it adjusted, and are harmless.

### Talk to it

The endpoint is reachable only inside the cluster in v0.1, so forward a port. Leave this running in
a second terminal:

```bash
./podserve port-forward qwen3-8b
```

Then:

```bash
curl localhost:8000/v1/models
curl localhost:8000/v1/chat/completions -H 'Content-Type: application/json' -d '{"model":"Qwen/Qwen3-8B","messages":[{"role":"user","content":"In one sentence: what is Kubernetes? /no_think"}],"max_tokens":80,"temperature":0}'
```

- The model name in requests is the Hugging Face repo id.
- Streaming (`"stream": true`) works too.
- `/no_think` turns off Qwen3's reasoning block for a short answer.

`./podserve list` shows what's deployed, with its GPUs, phase and endpoint.

### The dashboard

In the Cloud Console, open *Monitoring → Dashboards → podserve - model service* and pick the model
in the `model_name` filter at the top. It shows:
- time to first token, inter-token latency and end-to-end latency (p50/p95/p99)
- requests per second, and prompt and generation tokens per second
- requests running and waiting, and KV cache use
- prefix-cache hit rate and preemptions

### Optional: a bigger model on two GPUs

```bash
./podserve deploy hf://Qwen/Qwen3-14B
```

The plan picks two L4s with tensor parallelism and the model's full 40,960-token context. It took
20:22 to Ready the first time. When you're done with it, `./podserve delete qwen3-14b`.

## 8. Clean up

```bash
./podserve delete qwen3-8b
terraform -chdir=deploy/terraform destroy
```

- **`podserve delete`** removes the model's objects. Its pods are gone in seconds, and Autopilot
  releases the GPU machine about two minutes later. The cached weights stay in the bucket.
- **`destroy`** asks for `yes` and takes about **6 minutes**. It removes everything `apply` created,
  model bucket included.
- **What remains:**
  - the Terraform state bucket from step 4: `gcloud storage rm -r gs://$PROJECT-podserve-tfstate`
    removes it;
  - the APIs Terraform enabled, which stay on.

## Gated models

Some models, such as Llama and Gemma, need you to accept a licence on huggingface.co first. To
serve one:

1. Accept the licence on the model's Hugging Face page.
2. Make the token available to podserve, in two places:

```bash
export HF_TOKEN=hf_...   # a token from huggingface.co/settings/tokens
printf %s "$HF_TOKEN" | gcloud secrets versions add podserve-hf-token --project $PROJECT --data-file=-
```

- The CLI uses `HF_TOKEN` to read the model's config when planning.
- The fetch Job reads the secret when it copies the weights. Only the fetch Job's Kubernetes
  service account is granted access to it; the serving pods are not.

This path is covered by tests but hasn't yet been run against the live Hub.

## When something goes wrong

| What you see | What it means |
|---|---|
| `deploy` sits at `node` for a long time, and `kubectl -n podserve get events` shows `FailedScaleUp` | No spot L4 is free in your region right now. Autopilot keeps trying. Wait, try another region, or deploy on demand with `--spot=false` (it needs on-demand L4 quota). |
| `podserve: platform not initialised` | Run `./podserve init --config platform.json` against this cluster. |
| `podserve plan` refuses a model | The message names the reason: not a text-generation model, an architecture vLLM doesn't support, no safetensors, gated without a token, or too big for 8 L4s. |
| `deploy` times out | The message names the stage. `kubectl -n podserve logs deploy/<name>` shows vLLM's log; `kubectl -n podserve get pods,events` shows scheduling. |
| `terraform apply` fails on an organisation policy constraint | Your organisation blocks something the platform needs. Please open an issue with the constraint's name. |

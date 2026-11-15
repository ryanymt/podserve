"""podserve fetch job: copy one pinned Hugging Face revision into GCS.

The contract (environment variables, behaviour, progress lines, termination log, exit codes) is the
"fetch Job interface" section of docs/design/platform.md. Every progress line on stdout is one JSON
object; the final event (cached / done / error) is also written to /dev/termination-log so pkg/kube
can report the outcome without reading logs.

Run as `python -I /app/fetch.py` (isolated mode: the script's directory is not on sys.path).

The seam the tests use (images/fetch/test_fetch.py):
    run(env, hub, store, secrets, out, termination_log) -> exit code
    hub.model_info(repo, revision, token)               -> (gated: bool, files: [(name, size), ...])
    hub.download(repo, revision, name, dest_dir, token) -> local path of the downloaded file
    store.exists(bucket, object_name)                   -> bool
    store.upload(bucket, object_name, local_path)       -> size of the uploaded object
    store.write_text(bucket, object_name, text)
    secrets.latest(secret_name)                         -> str, or None when the secret has no version
main() builds the real clients (huggingface_hub, google-cloud-storage,
google-cloud-secret-manager) behind those methods and calls run(os.environ, ...). They are imported
inside the adapters so the tests can import this module without them.
"""
import json
import os
import sys
import time

# Top-level files copied from a revision: pkg/platform.FetchFileAllowed. Weights are safetensors
# only (D-015); configs, tokenizers and chat templates come along.
ALLOWED_SUFFIXES = (".safetensors", ".json", ".txt", ".model", ".jinja", ".tiktoken")
MARKER = "_PODSERVE_COMPLETE"


def _file_allowed(name):
    if "/" in name:  # top-level files only; subfolders (e.g. original/) are not served
        return False
    return name.endswith(ALLOWED_SUFFIXES)


def _dest_parts(dest):
    """PODSERVE_DEST `gs://BUCKET/PREFIX` (no trailing slash) -> (bucket, prefix)."""
    _, sep, rest = dest.partition("gs://")
    bucket, sep, prefix = rest.partition("/")
    prefix = prefix.rstrip("/")
    if not sep or not bucket or not prefix:
        raise ValueError(f"PODSERVE_DEST {dest!r}: want gs://BUCKET/PREFIX")
    return bucket, prefix


def _emit(out, event):
    out.write(json.dumps(event, separators=(",", ":")) + "\n")
    out.flush()


def _copy(env, hub, store, secrets, out):
    """The copy itself. Emits progress as it goes and returns (exit code, final event)."""
    started = time.monotonic()
    try:
        bucket, prefix = _dest_parts(env["PODSERVE_DEST"])
        # 1. Already copied: nothing to do, no Hub or Secret Manager calls.
        if store.exists(bucket, prefix + "/" + MARKER):
            return 0, {"event": "cached"}
        # 2. The token: latest secret version, stripped; none or whitespace-only means no token.
        token = secrets.latest(env["PODSERVE_HF_TOKEN_SECRET"])
        if token is not None:
            token = token.strip() or None
        repo, revision = env["PODSERVE_REPO"], env["PODSERVE_REVISION"]
        # 3. List the revision's files.
        gated, files = hub.model_info(repo, revision, token)
        if gated and token is None:
            return 3, {"event": "error", "code": "GATED_NO_TOKEN"}
        # 4. Filter like pkg/platform.FetchFileAllowed; weights must be among them.
        selected = [(name, size) for name, size in files if _file_allowed(name)]
        if not any(name.endswith(".safetensors") for name, _ in selected):
            return 4, {"event": "error", "code": "NO_SAFETENSORS"}
        # 5. One file at a time: download, upload, check the size, drop the local copy.
        scratch = env["PODSERVE_SCRATCH"]
        copied, total = [], 0
        for name, size in selected:
            began = time.monotonic()
            local = hub.download(repo, revision, name, scratch, token)
            uploaded = store.upload(bucket, prefix + "/" + name, local)
            if uploaded != size:
                return 5, {"event": "error", "code": "SIZE_MISMATCH", "file": name}
            try:
                os.remove(local)
            except OSError:
                pass  # the upload is verified; scratch is sized for one file, not a second try
            copied.append({"name": name, "size": size})
            total += uploaded
            _emit(out, {"event": "file", "name": name, "bytes": uploaded,
                        "seconds": round(time.monotonic() - began, 3)})
        # 6. The marker goes last: it is what makes the revision servable.
        marker = {"repo": repo, "revision": revision, "files": copied,
                  "podserve": env.get("PODSERVE_VERSION") or "dev"}
        store.write_text(bucket, prefix + "/" + MARKER,
                         json.dumps(marker, separators=(",", ":")))
        return 0, {"event": "done", "files": len(copied), "bytes": total,
                   "seconds": round(time.monotonic() - started, 3)}
    except Exception as exc:  # anything the steps above didn't expect
        return 1, {"event": "error", "code": "UNEXPECTED", "message": str(exc)}


def run(env, hub, store, secrets, out, termination_log="/dev/termination-log"):
    """Copy one pinned revision into GCS; returns the exit code (docs/design/platform.md)."""
    code, final = _copy(env, hub, store, secrets, out)
    _emit(out, final)
    try:  # a failure here must never change the exit code
        with open(termination_log, "w") as f:
            f.write(json.dumps(final, separators=(",", ":")) + "\n")
    except OSError:
        pass
    return code


class Hub:
    """huggingface_hub behind hub.model_info / hub.download."""

    def model_info(self, repo, revision, token):
        from huggingface_hub import HfApi
        info = HfApi().model_info(repo, revision=revision, files_metadata=True, token=token)
        # gated is False, "auto" or "manual"; any of the strings means a token is required.
        gated = bool(info.gated)
        files = [(s.rfilename, s.size if s.size is not None else 0) for s in info.siblings]
        return gated, files

    def download(self, repo, revision, name, dest_dir, token):
        from huggingface_hub import hf_hub_download
        return hf_hub_download(repo, name, revision=revision, local_dir=dest_dir, token=token)


class Store:
    """google.cloud.storage behind store.exists / store.upload / store.write_text."""

    def __init__(self):
        from google.cloud import storage
        self._client = storage.Client()

    def _blob(self, bucket, name):
        return self._client.bucket(bucket).blob(name)

    def exists(self, bucket, name):
        return self._blob(bucket, name).exists()

    def upload(self, bucket, name, local_path):
        blob = self._blob(bucket, name)
        blob.upload_from_filename(local_path, timeout=3600)
        blob.reload()  # the size as GCS stored it, not what we sent
        return blob.size

    def write_text(self, bucket, name, text):
        self._blob(bucket, name).upload_from_string(text, content_type="application/json")


class Secrets:
    """google.cloud.secretmanager behind secrets.latest."""

    def __init__(self):
        from google.cloud import secretmanager
        self._client = secretmanager.SecretManagerServiceClient()

    def latest(self, name):
        from google.api_core import exceptions
        try:
            version = self._client.access_secret_version(name=f"{name}/versions/latest")
        except (exceptions.NotFound, exceptions.FailedPrecondition):
            return None  # no version, or a disabled one: run without a token
        return version.payload.data.decode()


def main() -> int:
    return run(os.environ, Hub(), Store(), Secrets(), sys.stdout)


if __name__ == "__main__":
    sys.exit(main())

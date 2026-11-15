"""Spine test (P01 T2) for the fetch job, parked under testdata/spine/fetch/. T7 moves it to
images/fetch/test_fetch.py with `git mv`, unchanged.

It pins fetch.run() to the "fetch Job interface" in docs/design/platform.md, using fake Hub, storage
and Secret Manager clients. The environment comes from T6's rendered fetch Job
(testdata/render/qwen3-8b.golden.yaml), so the Go side and the Python side can't drift apart.

fetch.py's seam (T7 implements it; main() wires the real clients):
    run(env, hub, store, secrets, out, termination_log) -> exit code
    hub.model_info(repo, revision, token)            -> (gated: bool, files: [(name, size), ...])
    hub.download(repo, revision, name, dest_dir, token) -> local path of the downloaded file
    store.exists(bucket, object_name)                -> bool
    store.upload(bucket, object_name, local_path)    -> size of the uploaded object
    store.write_text(bucket, object_name, text)
    secrets.latest(secret_name)                      -> str, or None when the secret has no version
"""
import io
import json
import os
import pathlib
import tempfile
import unittest

import yaml

import fetch


def repo_root():
    p = pathlib.Path(__file__).resolve()
    for parent in p.parents:
        if (parent / "go.mod").exists():
            return parent
    raise RuntimeError("go.mod not found above " + str(p))


def golden_env():
    docs = list(yaml.safe_load_all((repo_root() / "testdata/render/qwen3-8b.golden.yaml").read_text()))
    job = docs[1]
    container = job["spec"]["template"]["spec"]["containers"][0]
    assert container["command"] == ["python", "-I", "/app/fetch.py"], container["command"]
    return {e["name"]: e["value"] for e in container["env"]}


ENV = golden_env()
BUCKET = "my-project-podserve-models"
PREFIX = "models/hf/Qwen/Qwen3-8B/b968826d9c46dd6066d109eabc6255188de91218"


class FakeHub:
    def __init__(self, files, gated=False, fail=None):
        self.files, self.gated, self.fail = files, gated, fail
        self.downloads, self.tokens = [], []

    def model_info(self, repo, revision, token):
        self.tokens.append(token)
        if self.fail:
            raise self.fail
        return self.gated, list(self.files)

    def download(self, repo, revision, name, dest_dir, token):
        self.downloads.append(name)
        path = os.path.join(dest_dir, name)
        with open(path, "wb") as f:
            f.write(b"x" * dict(self.files)[name])
        return path


class FakeStore:
    def __init__(self, objects=None, short=None):
        self.objects = dict(objects or {})
        self.calls = []
        self.short = short  # object name whose upload loses a byte

    def exists(self, bucket, name):
        self.calls.append(("exists", bucket, name))
        return (bucket, name) in self.objects

    def upload(self, bucket, name, local_path):
        size = os.path.getsize(local_path)
        if name.endswith("/" + (self.short or "\0")):
            size -= 1
        self.objects[(bucket, name)] = size
        self.calls.append(("upload", bucket, name, local_path))
        return size

    def write_text(self, bucket, name, text):
        self.objects[(bucket, name)] = text
        self.calls.append(("write_text", bucket, name))


class FakeSecrets:
    def __init__(self, value=None, fail=None):
        self.value, self.fail, self.asked = value, fail, []

    def latest(self, name):
        self.asked.append(name)
        if self.fail:
            raise self.fail
        return self.value


FILES = [
    (".gitattributes", 10), ("README.md", 20), ("config.json", 30), ("generation_config.json", 5),
    ("model-00001-of-00002.safetensors", 100), ("model-00002-of-00002.safetensors", 50),
    ("model.safetensors.index.json", 15), ("tokenizer.json", 40), ("tokenizer_config.json", 8),
    ("merges.txt", 12), ("vocab.json", 9), ("pytorch_model.bin", 999), ("original/consolidated.pth", 999),
    ("chat_template.jinja", 7),
]
ALLOWED = ["config.json", "generation_config.json", "model-00001-of-00002.safetensors",
           "model-00002-of-00002.safetensors", "model.safetensors.index.json", "tokenizer.json",
           "tokenizer_config.json", "merges.txt", "vocab.json", "chat_template.jinja"]


class FetchTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.term = os.path.join(self.tmp.name, "termination-log")
        self.env = dict(ENV, PODSERVE_SCRATCH=os.path.join(self.tmp.name, "scratch"))
        os.makedirs(self.env["PODSERVE_SCRATCH"])

    def run_fetch(self, hub, store, secrets):
        out = io.StringIO()
        code = fetch.run(self.env, hub, store, secrets, out, self.term)
        events = [json.loads(line) for line in out.getvalue().splitlines() if line.strip()]
        with open(self.term) as f:
            final = json.loads(f.read())
        return code, events, final

    def test_env_comes_from_the_rendered_job(self):
        self.assertEqual(ENV["PODSERVE_REPO"], "Qwen/Qwen3-8B")
        self.assertEqual(ENV["PODSERVE_DEST"], f"gs://{BUCKET}/{PREFIX}")
        self.assertEqual(set(ENV), {"PODSERVE_REPO", "PODSERVE_REVISION", "PODSERVE_DEST",
                                    "PODSERVE_HF_TOKEN_SECRET", "PODSERVE_SCRATCH", "HF_HUB_DISABLE_PROGRESS_BARS"})

    def test_cached_marker_short_circuits(self):
        store = FakeStore({(BUCKET, PREFIX + "/_PODSERVE_COMPLETE"): "{}"})
        hub = FakeHub(FILES)
        code, events, final = self.run_fetch(hub, store, FakeSecrets())
        self.assertEqual(code, 0)
        self.assertEqual(events, [{"event": "cached"}])
        self.assertEqual(final, {"event": "cached"})
        self.assertEqual(hub.downloads, [])

    def test_copies_allowed_files_one_at_a_time_and_writes_the_marker_last(self):
        store, hub = FakeStore(), FakeHub(FILES)
        code, events, final = self.run_fetch(hub, store, FakeSecrets(None))
        self.assertEqual(code, 0)
        self.assertEqual(hub.downloads, ALLOWED)  # Hub order, filtered
        uploads = [c for c in store.calls if c[0] == "upload"]
        self.assertEqual([c[2] for c in uploads], [PREFIX + "/" + n for n in ALLOWED])
        for c in uploads:
            self.assertFalse(os.path.exists(c[3]), "local copy not deleted after upload: " + c[3])
        self.assertEqual(store.calls[-1], ("write_text", BUCKET, PREFIX + "/_PODSERVE_COMPLETE"))
        marker = json.loads(store.objects[(BUCKET, PREFIX + "/_PODSERVE_COMPLETE")])
        self.assertEqual(marker["repo"], "Qwen/Qwen3-8B")
        self.assertEqual(marker["revision"], ENV["PODSERVE_REVISION"])
        self.assertEqual(marker["files"], [{"name": n, "size": dict(FILES)[n]} for n in ALLOWED])
        self.assertIn("podserve", marker)
        file_events = [e for e in events if e["event"] == "file"]
        self.assertEqual([e["name"] for e in file_events], ALLOWED)
        self.assertEqual([e["bytes"] for e in file_events], [dict(FILES)[n] for n in ALLOWED])
        done = events[-1]
        self.assertEqual(done["event"], "done")
        self.assertEqual(done["files"], len(ALLOWED))
        self.assertEqual(done["bytes"], sum(dict(FILES)[n] for n in ALLOWED))
        self.assertEqual(final["event"], "done")

    def test_no_secret_version_means_no_token(self):
        hub, secrets = FakeHub(FILES), FakeSecrets(None)
        code, _, _ = self.run_fetch(hub, FakeStore(), secrets)
        self.assertEqual(code, 0)
        self.assertEqual(secrets.asked, [ENV["PODSERVE_HF_TOKEN_SECRET"]])
        self.assertEqual(hub.tokens, [None])

    def test_empty_secret_means_no_token(self):
        hub = FakeHub(FILES)
        self.run_fetch(hub, FakeStore(), FakeSecrets("  \n"))
        self.assertEqual(hub.tokens, [None])

    def test_secret_value_is_the_token(self):
        hub = FakeHub(FILES)
        code, _, _ = self.run_fetch(hub, FakeStore(), FakeSecrets("hf_abc\n"))
        self.assertEqual(code, 0)
        self.assertEqual(hub.tokens, ["hf_abc"])

    def test_gated_without_token(self):
        code, events, final = self.run_fetch(FakeHub(FILES, gated=True), FakeStore(), FakeSecrets(None))
        self.assertEqual(code, 3)
        self.assertEqual(events[-1], {"event": "error", "code": "GATED_NO_TOKEN"})
        self.assertEqual(final, {"event": "error", "code": "GATED_NO_TOKEN"})

    def test_gated_with_token_is_fine(self):
        code, _, _ = self.run_fetch(FakeHub(FILES, gated=True), FakeStore(), FakeSecrets("hf_abc"))
        self.assertEqual(code, 0)

    def test_no_safetensors(self):
        files = [(n, s) for n, s in FILES if not n.endswith(".safetensors")]
        store = FakeStore()
        code, events, final = self.run_fetch(FakeHub(files), store, FakeSecrets(None))
        self.assertEqual(code, 4)
        self.assertEqual(final, {"event": "error", "code": "NO_SAFETENSORS"})
        self.assertEqual([c for c in store.calls if c[0] != "exists"], [])

    def test_size_mismatch_stops_before_the_marker(self):
        store = FakeStore(short="tokenizer.json")
        code, events, final = self.run_fetch(FakeHub(FILES), store, FakeSecrets(None))
        self.assertEqual(code, 5)
        self.assertEqual(final["event"], "error")
        self.assertEqual(final["code"], "SIZE_MISMATCH")
        self.assertNotIn((BUCKET, PREFIX + "/_PODSERVE_COMPLETE"), store.objects)

    def test_unexpected_error(self):
        code, events, final = self.run_fetch(FakeHub(FILES, fail=RuntimeError("boom")), FakeStore(), FakeSecrets(None))
        self.assertEqual(code, 1)
        self.assertEqual(final["event"], "error")
        self.assertEqual(final["code"], "UNEXPECTED")
        self.assertIn("boom", final["message"])

    def test_secret_manager_error_is_fatal(self):
        code, _, final = self.run_fetch(FakeHub(FILES), FakeStore(), FakeSecrets(fail=PermissionError("denied")))
        self.assertEqual(code, 1)
        self.assertEqual(final["code"], "UNEXPECTED")


if __name__ == "__main__":
    unittest.main()

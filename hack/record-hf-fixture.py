"""Record Hugging Face Hub responses as test fixtures (spine tool, P01 T2).

Usage: python3 -I hack/record-hf-fixture.py OUTDIR ORG/MODEL [ORG/MODEL ...]

For each repo, resolves `main` to a commit SHA and writes OUTDIR/ORG__MODEL/:
  info.json    GET /api/models/{repo}/revision/{sha}?blobs=true, trimmed to the fields podserve reads
               (id, sha, gated, pipeline_tag, cardData.license, siblings[].rfilename/.size)
  config.json  GET /{repo}/resolve/{sha}/config.json        (absent when the Hub refused it)
  index.json   GET /{repo}/resolve/{sha}/model.safetensors.index.json (if listed), reduced to its
               weight_map with one tensor per file: podserve reads only the set of files, and
               big models list tens of thousands of tensors
  meta.json    {"repo", "sha", "recorded", "configStatus", "indexStatus"}: HTTP status codes seen
Anonymous requests only: a gated model's config is recorded as refused (configStatus 401/403).
Uses only the standard library.
"""
import datetime
import json
import os
import sys
import urllib.error
import urllib.request

HUB = "https://huggingface.co"


def get(url):
    req = urllib.request.Request(url, headers={"User-Agent": "podserve-fixture-recorder"})
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, b""


def record(outdir, repo):
    status, body = get(f"{HUB}/api/models/{repo}/revision/main?blobs=true")
    if status != 200:
        raise SystemExit(f"{repo}: model info returned {status}")
    sha = json.loads(body)["sha"]
    status, body = get(f"{HUB}/api/models/{repo}/revision/{sha}?blobs=true")
    if status != 200:
        raise SystemExit(f"{repo}@{sha}: model info returned {status}")
    info = json.loads(body)
    trimmed = {
        "id": info.get("id"),
        "sha": info.get("sha"),
        "gated": info.get("gated", False),
        "pipeline_tag": info.get("pipeline_tag"),
        "cardData": {"license": (info.get("cardData") or {}).get("license")},
        "siblings": [{"rfilename": s["rfilename"], "size": s.get("size")} for s in info.get("siblings", [])],
    }
    d = os.path.join(outdir, repo.replace("/", "__"))
    os.makedirs(d, exist_ok=True)
    with open(os.path.join(d, "info.json"), "w") as f:
        json.dump(trimmed, f, indent=1, sort_keys=True)
        f.write("\n")

    cstatus, cbody = get(f"{HUB}/{repo}/resolve/{sha}/config.json")
    if cstatus == 200:
        with open(os.path.join(d, "config.json"), "w") as f:
            json.dump(json.loads(cbody), f, indent=1, sort_keys=True)
            f.write("\n")

    istatus = None
    if any(s["rfilename"] == "model.safetensors.index.json" for s in trimmed["siblings"]):
        istatus, ibody = get(f"{HUB}/{repo}/resolve/{sha}/model.safetensors.index.json")
        if istatus == 200:
            full = json.loads(ibody).get("weight_map", {})
            weight_map, seen = {}, set()
            for k in sorted(full):
                if full[k] not in seen:
                    seen.add(full[k])
                    weight_map[k] = full[k]
            with open(os.path.join(d, "index.json"), "w") as f:
                json.dump({"weight_map": weight_map}, f, indent=1, sort_keys=True)
                f.write("\n")

    meta = {"repo": repo, "sha": sha, "configStatus": cstatus, "indexStatus": istatus,
            "recorded": datetime.date.today().isoformat()}
    with open(os.path.join(d, "meta.json"), "w") as f:
        json.dump(meta, f, indent=1, sort_keys=True)
        f.write("\n")
    print(f"{repo:45} {sha[:12]} config={cstatus} index={istatus} files={len(trimmed['siblings'])}")


if __name__ == "__main__":
    if len(sys.argv) < 3:
        raise SystemExit(__doc__)
    for r in sys.argv[2:]:
        record(sys.argv[1], r)

#!/usr/bin/env bash
# The ModelService field table in docs/design/modelservice.md is hand-written. This checks it against
# the generated CRD (`go run ./hack/specfields`, design-vs-code pair C1): the same paths, the same
# types (Go names in the doc: int32 = integer, bool = boolean), and the same defaults. A default
# written in backticks must equal the CRD's default (a trailing "(choose)" note is ignored); one
# written in prose, such as "(required)" or "the repo id `ORG/MODEL`", means the CRD has none.
set -euo pipefail
cd "$(dirname "$0")/.."
extract=$(go run ./hack/specfields)
python3 -I - "docs/design/modelservice.md" <<PY
import re, sys
doc = open(sys.argv[1]).read()
block = doc.split("<!-- specfields:begin", 1)[1].split("<!-- specfields:end -->", 1)[0]
def rows(text):
    out = {}
    for line in text.splitlines():
        cells = [c.strip() for c in line.strip().strip("|").split("|")]
        if len(cells) >= 3 and cells[0].startswith("\`spec."):
            out[cells[0].strip("\`")] = (cells[1], cells[2])
    return out
code = rows("""$extract""")
want = rows(block)
types = {"int32": "integer", "bool": "boolean"}
errs = []
for path in sorted(set(code) | set(want)):
    if path not in want: errs.append(f"{path}: in the CRD, missing from the table"); continue
    if path not in code: errs.append(f"{path}: in the table, missing from the CRD"); continue
    dt, dd = want[path]; ct, cd = code[path]
    if types.get(dt, dt) != ct: errs.append(f"{path}: type {dt} in the table, {ct} in the CRD")
    m = re.fullmatch(r"\`([^\`]*)\`(?: \(choose\))?", dd)
    expect = m.group(1) if m else ""
    if expect != cd: errs.append(f"{path}: default {dd!r} in the table, {cd!r} in the CRD")
for e in errs: print("specfields:", e)
if errs: sys.exit(1)
print(f"specfields: ok ({len(code)} fields)")
PY

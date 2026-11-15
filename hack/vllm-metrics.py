"""Print the Prometheus series names a vLLM release exposes, one per line, sorted.

Usage: python3 -I hack/vllm-metrics.py <path to vllm/v1/metrics/loggers.py>

Parses the file with `ast` (never imports or executes it). It finds every call to
`self._gauge_cls(...)`, `self._counter_cls(...)` or `self._histogram_cls(...)` with a literal
`name="vllm:..."`, and prints the names Prometheus actually exposes for that metric type:
- gauge:     the name as declared
- counter:   the name with `_total` (prometheus_client appends it unless the name already ends so)
             and `_created` (the series start time)
- histogram: the name with `_bucket`, `_sum`, `_count` and `_created`
Dashboards must query these exposed names, not the declared ones.
"""
import ast
import sys

SUFFIXES = {
    "_gauge_cls": ("",),
    "_counter_cls": ("_total", "_created"),
    "_histogram_cls": ("_bucket", "_sum", "_count", "_created"),
}


def exposed_names(source: str) -> list[str]:
    names = set()
    for node in ast.walk(ast.parse(source)):
        if not (isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)):
            continue
        kind = node.func.attr
        if kind not in SUFFIXES:
            continue
        for kw in node.keywords:
            if kw.arg == "name" and isinstance(kw.value, ast.Constant) and isinstance(kw.value.value, str):
                name = kw.value.value
                if not name.startswith("vllm:"):
                    continue
                base = name
                if kind == "_counter_cls" and base.endswith("_total"):
                    base = base[: -len("_total")]  # prometheus_client strips it, then re-adds it
                for suffix in SUFFIXES[kind]:
                    names.add(base + suffix)
    if not names:
        raise SystemExit("no vllm: metrics found")
    return sorted(names)


if __name__ == "__main__":
    if len(sys.argv) != 2:
        raise SystemExit(__doc__)
    with open(sys.argv[1], encoding="utf-8") as f:
        for name in exposed_names(f.read()):
            print(name)

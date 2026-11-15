"""Print the text-generation architectures a vLLM release supports, one per line, sorted.

Usage: python3 -I hack/vllm-archs.py <path to vllm/model_executor/models/registry.py>

Reads the `_TEXT_GENERATION_MODELS` dict literal with `ast` (the file is parsed, never imported or
executed) and prints its keys. Only that table is used: vLLM's registry also lists multimodal,
embedding, reward, classification and speculative-decoding architectures, which podserve v0.1 must
refuse (P01 AC2).
"""
import ast
import sys


def text_generation_architectures(source: str) -> list[str]:
    tree = ast.parse(source)
    for node in tree.body:
        target = None
        if isinstance(node, ast.Assign) and len(node.targets) == 1:
            target, value = node.targets[0], node.value
        elif isinstance(node, ast.AnnAssign):
            target, value = node.target, node.value
        if isinstance(target, ast.Name) and target.id == "_TEXT_GENERATION_MODELS":
            if not isinstance(value, ast.Dict):
                raise SystemExit("_TEXT_GENERATION_MODELS is not a dict literal")
            keys = [k.value for k in value.keys if isinstance(k, ast.Constant) and isinstance(k.value, str)]
            if len(keys) != len(value.keys):
                raise SystemExit("_TEXT_GENERATION_MODELS has a non-string key")
            return sorted(set(keys))
    raise SystemExit("_TEXT_GENERATION_MODELS not found")


if __name__ == "__main__":
    if len(sys.argv) != 2:
        raise SystemExit(__doc__)
    with open(sys.argv[1], encoding="utf-8") as f:
        for arch in text_generation_architectures(f.read()):
            print(arch)

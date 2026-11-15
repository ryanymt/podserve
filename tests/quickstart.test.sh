#!/usr/bin/env bash
# The README's quickstart and docs/quickstart.md must not drift apart (P01 T12).
#
# Every command line in the README's "## Quickstart" section (the lines of its ```bash blocks,
# minus comments and blank lines) must appear, character for character and in the same order, as a
# line of a ```bash block in docs/quickstart.md. docs/quickstart.md may add optional commands in
# between; the README may not have a command that docs/quickstart.md lacks.
set -euo pipefail
cd "$(dirname "$0")/.."

commands() { # FILE [SECTION-HEADING-PREFIX]: the command lines of FILE's ```bash blocks
  awk -v sect="${2:-}" '
    sect != "" && /^## / { inside = (index($0, sect) == 1) }
    sect == "" { inside = 1 }
    /^```bash$/ { if (inside) code = 1; next }
    /^```/      { code = 0; next }
    code && $0 !~ /^[[:space:]]*(#|$)/ { print }
  ' "$1"
}

readme=$(commands README.md "## Quickstart")
guide=$(commands docs/quickstart.md)
[ -n "$readme" ] || { echo "FAIL: no commands found in README.md's ## Quickstart section"; exit 1; }

# In-order subsequence check.
python3 -I - "$readme" "$guide" <<'EOF'
import sys
readme, guide = sys.argv[1].splitlines(), sys.argv[2].splitlines()
i = 0
for line in readme:
    try:
        i = guide.index(line, i) + 1
    except ValueError:
        where = "at all" if line not in guide else "in this order"
        sys.exit(f"FAIL: README quickstart command not in docs/quickstart.md {where}:\n  {line}")
print(f"ok: {len(readme)} README quickstart commands appear in docs/quickstart.md, in order")
EOF

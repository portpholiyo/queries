#!/usr/bin/env python3
"""CI guard against documentation drift. Stdlib only.
1. Every backticked repo path in docs/architecture.md must exist.
2. Every relative markdown link in README.md and docs/**/*.md must resolve.
3. Every mermaid block must have a recognised diagram type (syntax is checked by GitHub's renderer)."""
import re, sys
from pathlib import Path

root = Path(__file__).resolve().parent.parent
bad = []
arch = (root / "docs/architecture.md").read_text()
for p in re.findall(r"`((?:cmd|internal)/[^`\s]+|Dockerfile)`", arch):
    if not (root / p).exists():
        bad.append(f"architecture.md mentions missing path: {p}")
for f in [root / "README.md", *root.glob("docs/**/*.md")]:
    for link in re.findall(r"\]\((?!https?:|#|mailto:)([^)#]+)", f.read_text()):
        if not (f.parent / link).resolve().exists():
            bad.append(f"{f.relative_to(root)}: broken link {link}")
    for blk in re.findall(r"```mermaid\n(.*?)```", f.read_text(), re.S):
        if not re.match(r"\s*(flowchart|graph|sequenceDiagram|classDiagram|stateDiagram|erDiagram|gantt)\b", blk):
            bad.append(f"{f.relative_to(root)}: unknown mermaid diagram type")
print("\n".join(bad) or "docs ok")
sys.exit(1 if bad else 0)

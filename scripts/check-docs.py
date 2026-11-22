#!/usr/bin/env python3
"""CI guard for the public docs. Stdlib only. One copy per public repo at scripts/check-docs.py
(canonical copy: tools/docs-standard/check-docs.py in the private workspace).

1. Every backticked repo path in docs/architecture.md must exist.
2. Every relative markdown link in README.md and docs/**/*.md must resolve.
3. Every mermaid block must have a recognised diagram type (syntax is checked by GitHub's renderer).
4. docs/architecture.md follows the common standard below. Project-specific extra `##` sections are
   allowed anywhere; the required ones must exist, in this order:
     Context                 flowchart/graph diagram
     Components              a table (path | responsibility)
     <flows...>              at least one sequenceDiagram (project-specific headings)
     Data                    erDiagram
     Limits...               heading starts with "Limits"
     Security and privacy
     Testing strategy
"""
import re, sys
from pathlib import Path

root = Path(__file__).resolve().parent.parent
bad = []

arch_path = root / "docs/architecture.md"
arch = arch_path.read_text()

# 1. paths mentioned in the architecture doc exist
for p in re.findall(r"`((?:cmd|internal)/[^`\s]+|Dockerfile)`", arch):
    if not (root / p).exists():
        bad.append(f"architecture.md mentions missing path: {p}")

# 2 + 3. links and mermaid types
for f in [root / "README.md", *root.glob("docs/**/*.md")]:
    txt = f.read_text()
    for link in re.findall(r"\]\((?!https?:|#|mailto:)([^)#]+)", txt):
        if not (f.parent / link).resolve().exists():
            bad.append(f"{f.relative_to(root)}: broken link {link}")
    for blk in re.findall(r"```mermaid\n(.*?)```", txt, re.S):
        if not re.match(r"\s*(flowchart|graph|sequenceDiagram|classDiagram|stateDiagram|erDiagram|gantt)\b", blk):
            bad.append(f"{f.relative_to(root)}: unknown mermaid diagram type")

# 4. common architecture standard
parts = re.split(r"(?m)^## (.+)$", arch)  # [preamble, title1, body1, title2, body2, ...]
sections = [(parts[i].strip(), parts[i + 1]) for i in range(1, len(parts) - 1, 2)]
titles = [t for t, _ in sections]


def find(prefix):
    for i, (t, _) in enumerate(sections):
        if t.lower().startswith(prefix.lower()):
            return i
    return None


def diagram_types(body):
    return [re.match(r"\s*(\w+)", b).group(1) for b in re.findall(r"```mermaid\n(.*?)```", body, re.S)]


idx = {}
for name in ["Context", "Components", "Data", "Limits", "Security and privacy", "Testing strategy"]:
    i = find(name)
    if i is None:
        bad.append(f"architecture.md standard: missing required section '## {name}'")
    idx[name] = i
present = [(n, i) for n, i in idx.items() if i is not None]
if [i for _, i in present] != sorted(i for _, i in present):
    bad.append("architecture.md standard: required sections are out of order (expected Context, Components, <flows>, Data, Limits, Security and privacy, Testing strategy)")
if idx["Context"] is not None and not set(diagram_types(sections[idx["Context"]][1])) & {"flowchart", "graph"}:
    bad.append("architecture.md standard: 'Context' needs a flowchart diagram")
if idx["Components"] is not None and not re.search(r"(?m)^\|.+\|\s*$", sections[idx["Components"]][1]):
    bad.append("architecture.md standard: 'Components' needs a table (path | responsibility)")
if idx["Data"] is not None and "erDiagram" not in diagram_types(sections[idx["Data"]][1]):
    bad.append("architecture.md standard: 'Data' needs an erDiagram (entity-relationship diagram)")
if idx["Components"] is not None and idx["Data"] is not None:
    between = "".join(b for _, b in sections[idx["Components"] + 1 : idx["Data"]])
    if "sequenceDiagram" not in diagram_types(between):
        bad.append("architecture.md standard: add at least one flow section (between Components and Data) with a sequenceDiagram")

print("\n".join(bad) or "docs ok")
sys.exit(1 if bad else 0)

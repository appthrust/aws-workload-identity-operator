#!/usr/bin/env bash
set -euo pipefail

status=0

if [[ -e docs/superpowers ]]; then
  echo "docs/superpowers must not be present in public documentation" >&2
  status=1
fi

public_paths=(
  README.md
  docs
  charts/aws-workload-identity-operator/README.md
)

if rg -n '(TODO|TBD|WIP|Expected: FAIL|REQUIRED SUB-SKILL)' "${public_paths[@]}"; then
  echo "public docs contain internal planning markers" >&2
  status=1
fi

if rg -n '<chart-version>' "${public_paths[@]}"; then
  echo "public docs contain stale <chart-version> placeholders" >&2
  status=1
fi

if ! python3 - <<'PY'
from __future__ import annotations

import re
import sys
from pathlib import Path
from urllib.parse import unquote, urlparse

root = Path.cwd()
markdown_files = [root / "README.md"]
markdown_files.extend(sorted((root / "docs").rglob("*.md")))
markdown_files.append(root / "charts/aws-workload-identity-operator/README.md")

inline_link = re.compile(r"(?<!!)\[[^\]\n]+\]\(([^)\s]+)(?:\s+\"[^\"]*\")?\)")
reference_link = re.compile(r"^\s*\[[^\]]+\]:\s+(\S+)", re.MULTILINE)
heading = re.compile(r"^(#{1,6})\s+(.+?)\s*#*\s*$", re.MULTILINE)
html_id = re.compile(r"<a\s+(?:[^>]*\s+)?(?:id|name)=[\"']([^\"']+)[\"']", re.IGNORECASE)


def github_anchor(text: str) -> str:
    text = re.sub(r"`([^`]*)`", r"\1", text)
    text = re.sub(r"<[^>]+>", "", text)
    text = text.strip().lower()
    text = re.sub(r"[^\w\- ]+", "", text)
    text = re.sub(r"\s+", "-", text)
    return text


def anchors_for(path: Path) -> set[str]:
    content = path.read_text(encoding="utf-8")
    anchors: set[str] = set(html_id.findall(content))
    seen: dict[str, int] = {}
    for match in heading.finditer(content):
        base = github_anchor(match.group(2))
        if not base:
            continue
        count = seen.get(base, 0)
        seen[base] = count + 1
        anchors.add(base if count == 0 else f"{base}-{count}")
    return anchors


anchor_cache: dict[Path, set[str]] = {}


def target_anchors(path: Path) -> set[str]:
    path = path.resolve()
    if path not in anchor_cache:
        anchor_cache[path] = anchors_for(path)
    return anchor_cache[path]


def local_targets(content: str) -> list[str]:
    return [m.group(1) for m in inline_link.finditer(content)] + [
        m.group(1) for m in reference_link.finditer(content)
    ]


def is_external(target: str) -> bool:
    parsed = urlparse(target)
    return bool(parsed.scheme) and parsed.scheme not in {"", "file"}


errors: list[str] = []
for md in markdown_files:
    if not md.exists():
        errors.append(f"{md.relative_to(root)}: markdown file does not exist")
        continue

    content = md.read_text(encoding="utf-8")
    for raw_target in local_targets(content):
        target = raw_target.strip()
        if not target or is_external(target):
            continue
        if target.startswith("mailto:") or target.startswith("tel:"):
            continue

        parsed = urlparse(target)
        if parsed.scheme == "file":
            errors.append(f"{md.relative_to(root)}: file URI is not allowed: {target}")
            continue

        link_path = unquote(parsed.path)
        fragment = unquote(parsed.fragment)

        if link_path:
            candidate = (md.parent / link_path).resolve()
        else:
            candidate = md.resolve()

        try:
            candidate.relative_to(root)
        except ValueError:
            errors.append(f"{md.relative_to(root)}: link escapes repository: {target}")
            continue

        if not candidate.exists():
            errors.append(f"{md.relative_to(root)}: broken link target: {target}")
            continue

        if candidate.is_dir():
            errors.append(f"{md.relative_to(root)}: link target is a directory: {target}")
            continue

        if fragment:
            anchors = target_anchors(candidate)
            normalized = github_anchor(fragment)
            if fragment not in anchors and normalized not in anchors:
                errors.append(
                    f"{md.relative_to(root)}: missing anchor #{fragment} in "
                    f"{candidate.relative_to(root)}"
                )

if errors:
    for error in errors:
        print(error, file=sys.stderr)
    sys.exit(1)
PY
then
  status=1
fi

exit "${status}"

#!/usr/bin/env python3
"""Make discovered provider groups explicitly selectable.

This is a deliberately narrow line-oriented transformation.  It refuses
unknown YAML shapes instead of trying to rewrite arbitrary mihomo config.
"""

from __future__ import annotations

import re
import os
from pathlib import Path


_GROUP_RE = re.compile(r"^( {2})-\s+name:\s*(.*?)\s*$")
_PROPERTY_RE = re.compile(r"^( {4})([A-Za-z0-9_-]+):(?:\s*(.*))?$")


def patch_provider_groups(text: str, main_group: str, backup_group: str) -> str:
    if not main_group:
        raise ValueError("main provider group is required")
    wanted = [main_group]
    if backup_group:
        wanted.append(backup_group)
    lines = text.splitlines(keepends=True)
    root = next((i for i, line in enumerate(lines) if line.rstrip("\r\n") == "proxy-groups:"), None)
    if root is None:
        raise ValueError("mihomo config has no proxy-groups mapping")

    groups: list[tuple[int, int, str]] = []
    for index in range(root + 1, len(lines)):
        raw = lines[index].rstrip("\r\n")
        if raw and not raw.startswith(" "):
            break
        match = _GROUP_RE.match(raw)
        if match:
            groups.append((index, len(lines), _unquote(match.group(2))))
    for index, (start, _, name) in enumerate(groups):
        end = groups[index + 1][0] if index + 1 < len(groups) else len(lines)
        groups[index] = (start, end, name)

    for wanted_name in wanted:
        matching = [item for item in groups if item[2] == wanted_name]
        if len(matching) != 1:
            raise ValueError(f"provider group {wanted_name!r} was not found uniquely")

    replacements: dict[int, str] = {}
    removals: set[int] = set()
    removable = {"url", "expected-status", "interval", "tolerance", "lazy"}
    for start, end, name in groups:
        if name not in wanted:
            continue
        found_type = False
        has_use = False
        for index in range(start + 1, end):
            raw = lines[index].rstrip("\r\n")
            match = _PROPERTY_RE.match(raw)
            if not match:
                continue
            key, value = match.group(2), (match.group(3) or "").strip()
            if key == "type":
                found_type = True
                if value not in {"select", "url-test", "fallback", "load-balance"}:
                    raise ValueError(f"provider group {name!r} has unsupported type {value!r}")
                replacements[index] = "    type: select\n"
            elif key == "use":
                has_use = True
            elif key in removable:
                removals.add(index)
        if not found_type:
            raise ValueError(f"provider group {name!r} has no type")
        if not has_use:
            # Static node lists are valid and can still be pinned.  Only the
            # provider-backed shape needs the use relationship for discovery.
            continue

    result = []
    for index, line in enumerate(lines):
        if index in removals:
            continue
        result.append(replacements.get(index, line))
    output = "".join(result)
    if output.count("    type: select") < len(wanted):
        raise ValueError("provider group patch validation failed")
    return output


def patch_file(path: str, main_group: str, backup_group: str) -> None:
    target = Path(path)
    original = target.read_text(encoding="utf-8")
    patched = patch_provider_groups(original, main_group, backup_group)
    temporary = target.with_name(target.name + ".guardian.tmp")
    temporary.write_text(patched, encoding="utf-8")
    mode = target.stat().st_mode & 0o7777
    temporary.chmod(mode)
    try:
        os.chown(temporary, target.stat().st_uid, target.stat().st_gid)
    except PermissionError:
        # The installer normally runs as root.  A non-root unit test can
        # still exercise the transformation when ownership cannot be copied.
        pass
    temporary.replace(target)


def _unquote(value: str) -> str:
    if len(value) >= 2 and value[0] == value[-1] and value[0] in "'\"":
        return value[1:-1]
    return value

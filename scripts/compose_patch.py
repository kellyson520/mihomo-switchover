#!/usr/bin/env python3
"""Conservatively inject guardian mounts into one compose service.

This module intentionally does not parse arbitrary YAML.  It operates on the
small, stable subset needed for a compose service and refuses structures it
cannot identify without guessing.
"""

from __future__ import annotations

import re
from pathlib import Path


_SERVICE_RE = re.compile(r"^( {2})([A-Za-z0-9_.-]+):\s*(.*)$")
_PROPERTY_RE = re.compile(r"^( {4})([A-Za-z0-9_.-]+):(?:\s*(.*))?$")
_CONTAINER_RE = re.compile(r"^\s{4}container_name:\s*['\"]?([^'\" #]+)")


def patch_compose(compose_text: str, guardian_root: str, compose_dir: str | None = None) -> str:
    """Return an idempotently patched compose document.

    ``guardian_root`` is the host directory that will be mounted at
    ``/guardian``.  A target service is selected only when exactly one service
    is named ``mihomo-cliproxy`` or has that exact ``container_name``.
    """

    lines = compose_text.splitlines(keepends=True)
    service_start, service_end = _find_target_service(lines)
    body = lines[service_start + 1 : service_end]
    if not body or any(line.strip() == "" for line in body[:1]):
        raise ValueError("mihomo-cliproxy service has no usable mapping body")
    if body[0].lstrip().startswith(("-", "[", "null")):
        raise ValueError("mihomo-cliproxy service must be a mapping")

    root = Path(guardian_root).resolve()
    compose_base = Path(compose_dir).resolve() if compose_dir else root.parent
    body = _replace_scalar_property(
        body,
        "entrypoint",
        'entrypoint: ["/bin/sh", "/guardian/start-guardian.sh"]',
    )
    body = _replace_scalar_property(body, "command", "command: []")
    body = _ensure_mounts(body, root)
    body = _normalize_relative_provider_mount(body, compose_base)
    patched = lines[: service_start + 1] + body + lines[service_end:]
    result = "".join(patched)
    _validate_result(result)
    return result


def _find_target_service(lines: list[str]) -> tuple[int, int]:
    service_header = next(
        (index for index, line in enumerate(lines) if line.rstrip("\r\n") == "services:"),
        None,
    )
    if service_header is None:
        raise ValueError("compose file has no top-level services mapping")

    starts: list[tuple[int, str, str]] = []
    for index in range(service_header + 1, len(lines)):
        raw = lines[index].rstrip("\r\n")
        if raw and not raw.startswith(" "):
            break
        match = _SERVICE_RE.match(raw)
        if match:
            starts.append((index, match.group(2), match.group(3).strip()))
    if not starts:
        raise ValueError("compose services mapping is empty")

    candidates: list[tuple[int, int]] = []
    for offset, (start, name, inline) in enumerate(starts):
        end = starts[offset + 1][0] if offset + 1 < len(starts) else len(lines)
        if name == "mihomo-cliproxy" or any(
            _CONTAINER_RE.match(line.rstrip("\r\n"))
            and _CONTAINER_RE.match(line.rstrip("\r\n")).group(1) == "mihomo-cliproxy"
            for line in lines[start + 1 : end]
        ):
            candidates.append((start, end))
    if len(candidates) != 1:
        if not candidates:
            raise ValueError("mihomo-cliproxy service was not found uniquely")
        raise ValueError("mihomo-cliproxy service was not found uniquely")
    return candidates[0]


def _replace_scalar_property(lines: list[str], key: str, replacement: str) -> list[str]:
    result: list[str] = []
    replaced = False
    index = 0
    while index < len(lines):
        raw = lines[index].rstrip("\r\n")
        match = _PROPERTY_RE.match(raw)
        if match and match.group(2) == key:
            if match.group(3) == "" or match.group(3) is None:
                index += 1
                while index < len(lines) and _indent(lines[index]) > 4:
                    index += 1
            else:
                index += 1
            if not replaced:
                result.append("    " + replacement + "\n")
                replaced = True
            continue
        result.append(lines[index])
        index += 1
    if not replaced:
        result.insert(0, "    " + replacement + "\n")
    return result


def _ensure_mounts(lines: list[str], root: Path) -> list[str]:
    mounts = [
        ("/guardian/start-guardian.sh", root / "start-guardian.sh", "ro"),
        ("/guardian/bin/guardian", root / "bin" / "guardian", "ro"),
        ("/guardian/guardian.yaml", root / "guardian.yaml", "ro"),
        ("/guardian/data", root / "data", "rw"),
        ("/guardian/logs", root / "logs", "rw"),
        ("/guardian/controller_secret", root / "controller_secret", "ro"),
    ]
    volume_index = None
    for index, line in enumerate(lines):
        match = _PROPERTY_RE.match(line.rstrip("\r\n"))
        if match and match.group(2) == "volumes":
            if match.group(3):
                raise ValueError("mihomo-cliproxy volumes must be a YAML list")
            volume_index = index
            break
    if volume_index is None:
        volume_index = _insert_before_next_property(lines, 0)
        lines[volume_index:volume_index] = ["    volumes:\n"]

    end = volume_index + 1
    while end < len(lines) and (_indent(lines[end]) > 4 or not lines[end].strip()):
        end += 1
    existing = lines[volume_index + 1 : end]
    for target, source, mode in mounts:
        canonical = f"      - {source}:{target}:{mode}\n"
        found = None
        for index, line in enumerate(existing):
            if _volume_target(line) == target:
                found = index
                break
        if found is None:
            existing.append(canonical)
        elif existing[found] != canonical:
            existing[found] = canonical
    return lines[: volume_index + 1] + existing + lines[end:]


def _normalize_relative_provider_mount(lines: list[str], compose_base: Path) -> list[str]:
    result = []
    for line in lines:
        target = _volume_target(line)
        if target != "/root/.config/mihomo/providers":
            result.append(line)
            continue
        match = re.match(r"^(\s*-\s+)([^:]+):([^:]+)(?::([^\s]+))?(\s*)$", line.rstrip("\r\n"))
        if not match:
            raise ValueError("provider mount has unsupported syntax")
        source = match.group(2)
        if source.startswith("./") or source.startswith("../"):
            source = str((compose_base / source).resolve())
        mode = f":{match.group(4)}" if match.group(4) else ""
        newline = "\n" if line.endswith("\n") else ""
        result.append(f"{match.group(1)}{source}:{target}{mode}{newline}")
    return result


def _insert_before_next_property(lines: list[str], start: int) -> int:
    index = start
    while index < len(lines):
        if index > start and _indent(lines[index]) == 4 and lines[index].strip():
            return index
        index += 1
    return len(lines)


def _volume_target(line: str) -> str | None:
    stripped = line.strip()
    if not stripped.startswith("-"):
        return None
    value = stripped[1:].strip().strip('"\'')
    parts = value.split(":")
    if len(parts) < 2:
        return None
    return parts[-2] if len(parts) >= 3 and parts[-1] in {"ro", "rw", "z", "Z"} else parts[-1]


def _indent(line: str) -> int:
    return len(line) - len(line.lstrip(" ")) if line.strip() else 99


def _validate_result(text: str) -> None:
    required = [
        'entrypoint: ["/bin/sh", "/guardian/start-guardian.sh"]',
        "command: []",
        "/guardian/start-guardian.sh",
        "/guardian/bin/guardian",
        "/guardian/guardian.yaml",
        "/guardian/data",
        "/guardian/logs",
        "/guardian/controller_secret",
    ]
    missing = [value for value in required if value not in text]
    if missing:
        raise ValueError("compose patch validation failed; missing " + ", ".join(missing))


if __name__ == "__main__":
    raise SystemExit("import patch_compose from scripts.compose_patch")

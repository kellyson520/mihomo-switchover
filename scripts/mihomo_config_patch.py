#!/usr/bin/env python3
"""Make discovered provider groups explicitly selectable.

This is a deliberately narrow line-oriented transformation.  It refuses
unknown YAML shapes instead of trying to rewrite arbitrary mihomo config.
"""

from __future__ import annotations

import re
import os
import json
from pathlib import Path
from typing import Any, Mapping, Sequence
from urllib.parse import urlsplit

from scripts.discover import _parse_yaml


_GROUP_RE = re.compile(r"^( {2})-\s+name:\s*(.*?)\s*$")
_PROPERTY_RE = re.compile(r"^( {4})([A-Za-z0-9_-]+):(?:\s*(.*))?$")
_QUALITY_ID_RE = re.compile(r"^[a-z0-9][a-z0-9_-]{0,31}$")
_QUALITY_GROUP_PREFIX = "GUARDIAN-QUALITY-"
_QUALITY_LISTENER_PREFIX = "guardian-quality-"
_QUALITY_OWNER_RE = re.compile(
    r"^#\s*mihomo-guardian:\s*generated quality target\s+"
    r"([a-z0-9][a-z0-9_-]{0,31})\s*$"
)


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
                replacements[index] = (
                    "    type: select\r\n"
                    if "\r\n" in text
                    else "    type: select\n"
                )
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


def patch_quality_targets(
    text: str,
    targets: Sequence[Mapping[str, object]],
) -> str:
    """Inject only guardian-owned quality groups and loopback listeners.

    The transformation deliberately parses only the two mihomo list sections.
    All other text is copied byte-for-byte.  Generated entries carry an owner
    comment; a same-name entry without that comment is treated as a user
    collision and is never overwritten.
    """

    if not isinstance(text, str):
        raise TypeError("mihomo config must be text")
    if not targets:
        return text

    normalised = _normalise_quality_targets(targets)
    parsed_config = _parse_yaml(text, "mihomo config")
    if not isinstance(parsed_config, Mapping):
        raise ValueError("mihomo config must be a mapping")
    declared_providers = parsed_config.get("proxy-providers")
    if declared_providers is not None and not isinstance(declared_providers, Mapping):
        raise ValueError("mihomo proxy-providers must be a mapping")
    declared_provider_names = (
        set(declared_providers) if isinstance(declared_providers, Mapping) else set()
    )
    for target in normalised:
        provider = _optional_string(target.get("provider"))
        if provider and provider not in declared_provider_names:
            raise ValueError(
                f"quality target {target['id']!r} references undeclared provider "
                f"{provider!r}; add it under top-level proxy-providers"
            )
    lines = text.splitlines(keepends=True)
    newline = "\r\n" if "\r\n" in text else "\n"

    group_bounds = _section_bounds(lines, "proxy-groups")
    if group_bounds is None:
        raise ValueError("mihomo config has no proxy-groups mapping")
    group_entries, group_blocks = _section_entries(
        lines, group_bounds, "proxy-groups"
    )
    group_names = _entry_names(group_entries, "proxy-groups")
    source_groups = {name: entry for name, entry in zip(group_names, group_entries)}

    listener_bounds = _section_bounds(lines, "listeners")
    if listener_bounds is None:
        listener_entries: list[dict[str, Any]] = []
        listener_blocks: list[tuple[int, int]] = []
    else:
        listener_entries, listener_blocks = _section_entries(
            lines, listener_bounds, "listeners"
        )
    listener_names = _entry_names(listener_entries, "listeners")

    wanted_group_names = {
        _QUALITY_GROUP_PREFIX + target["id"] for target in normalised
    }
    wanted_listener_names = {
        _QUALITY_LISTENER_PREFIX + target["id"] for target in normalised
    }
    _validate_owned_or_colliding(
        lines, group_blocks, group_names, "group", wanted_group_names
    )
    _validate_owned_or_colliding(
        lines, listener_blocks, listener_names, "listener", wanted_listener_names
    )

    for target in normalised:
        source_name = target["source_group"]
        if source_name not in source_groups:
            raise ValueError(
                f"quality target {target['id']!r} source group {source_name!r} "
                "does not exist exactly"
            )
        if source_name.startswith(_QUALITY_GROUP_PREFIX):
            raise ValueError("quality source_group must be a user-owned group")

    configured_ports = _configured_mihomo_ports(text)
    listener_ports: dict[str, int] = {}
    listener_port_names: dict[int, str] = {}
    user_listener_ports: set[int] = set()
    owned_listener_ports: set[int] = set()
    owned_listener_target_ports: dict[str, int] = {}
    for index, entry in enumerate(listener_entries):
        name = listener_names[index]
        port = _entry_port(entry, f"listeners[{index}]")
        if name in listener_ports:
            raise ValueError(f"duplicate listener name {name!r}")
        previous_name = listener_port_names.get(port)
        if previous_name is not None:
            raise ValueError(
                f"duplicate listener port {port} ({previous_name!r} and {name!r})"
            )
        listener_ports[name] = port
        listener_port_names[port] = name
        owner = _owner_id(lines, listener_blocks[index])
        if owner is not None:
            expected = _QUALITY_LISTENER_PREFIX + owner
            if name != expected:
                raise ValueError(
                    f"owned listener {name!r} does not match owner target {owner!r}"
                )
            owned_listener_ports.add(port)
            owned_listener_target_ports[owner] = port
        else:
            user_listener_ports.add(port)

    target_ports: dict[str, int] = {}
    target_ids = {target["id"] for target in normalised}
    for target in normalised:
        port = _listener_port(target["listener"], target["id"])
        if port in target_ports.values():
            raise ValueError(f"duplicate quality listener port {port}")
        if port in configured_ports:
            raise ValueError(
                f"quality listener port {port} conflicts with a mihomo config port"
            )
        if port in user_listener_ports:
            raise ValueError(
                f"quality listener port {port} conflicts with a user listener"
            )
        conflicting_owner = next(
            (
                owner
                for owner, owner_port in owned_listener_target_ports.items()
                if owner_port == port and owner != target["id"]
            ),
            None,
        )
        if conflicting_owner is not None and conflicting_owner in target_ids:
            raise ValueError(
                f"quality listener port {port} belongs to quality target {conflicting_owner!r}"
            )
        target_ports[target["id"]] = port

    removable_groups = _owned_indexes(
        lines, group_blocks, group_names, _QUALITY_GROUP_PREFIX
    )
    # Blocks are removed from their respective section below.  Keeping the
    # operation section-local avoids accidentally deleting unrelated YAML.
    new_lines = _remove_blocks(lines, group_blocks, removable_groups)
    group_bounds = _section_bounds(new_lines, "proxy-groups")
    assert group_bounds is not None

    generated_groups: list[str] = []
    for target in normalised:
        group_name = _QUALITY_GROUP_PREFIX + target["id"]
        provider = _optional_string(target.get("provider"))
        if provider:
            body = (
                f"  - name: {group_name}{newline}"
                f"    # mihomo-guardian: generated quality target {target['id']}{newline}"
                f"    type: select{newline}"
                f"    use:{newline}"
                f"      - {_yaml_scalar(provider)}{newline}"
            )
        else:
            source = source_groups[target["source_group"]]
            proxies = _string_sequence(
                source.get("proxies"),
                f"source group {target['source_group']!r}.proxies",
            )
            if not proxies:
                raise ValueError(
                    f"quality target {target['id']!r} requires a non-empty static proxy group"
                )
            body = (
                f"  - name: {group_name}{newline}"
                f"    # mihomo-guardian: generated quality target {target['id']}{newline}"
                f"    type: select{newline}"
                f"    proxies:{newline}"
                + "".join(
                    f"      - {_yaml_scalar(proxy)}{newline}" for proxy in proxies
                )
            )
        generated_groups.append(body)
    new_lines = _insert_section_blocks(
        new_lines, group_bounds, generated_groups, newline
    )

    if listener_bounds is not None:
        current_listener_bounds = _section_bounds(new_lines, "listeners")
        assert current_listener_bounds is not None
        _, current_listener_blocks = _section_entries(
            new_lines, current_listener_bounds, "listeners"
        )
        current_listener_names = _entry_names(
            _section_entries(new_lines, current_listener_bounds, "listeners")[0],
            "listeners",
        )
        current_removable_listeners = _owned_indexes(
            new_lines,
            current_listener_blocks,
            current_listener_names,
            _QUALITY_LISTENER_PREFIX,
        )
        new_lines = _remove_blocks(
            new_lines,
            current_listener_blocks,
            current_removable_listeners,
        )
        listener_bounds = _section_bounds(new_lines, "listeners")
        if listener_bounds is not None:
            listener_bounds = _expand_empty_list_section(
                new_lines, listener_bounds, "listeners", newline
            )
    generated_listeners: list[str] = []
    for target in normalised:
        listener_name = _QUALITY_LISTENER_PREFIX + target["id"]
        generated_listeners.append(
            f"  - name: {listener_name}{newline}"
            f"    # mihomo-guardian: generated quality target {target['id']}{newline}"
            f"    type: mixed{newline}"
            f"    listen: 127.0.0.1{newline}"
            f"    port: {target_ports[target['id']]}{newline}"
            f"    proxy: {_QUALITY_GROUP_PREFIX + target['id']}{newline}"
        )
    if listener_bounds is None:
        new_lines = _append_new_section(
            new_lines, "listeners", generated_listeners, newline
        )
    else:
        new_lines = _insert_section_blocks(
            new_lines, listener_bounds, generated_listeners, newline
        )

    result = "".join(new_lines)
    _validate_generated_quality_types(result, normalised)
    return result


def _normalise_quality_targets(
    targets: Sequence[Mapping[str, object]],
) -> list[dict[str, Any]]:
    result: list[dict[str, Any]] = []
    ids: set[str] = set()
    for index, raw in enumerate(targets):
        if not isinstance(raw, Mapping):
            raise ValueError(f"quality target {index} must be a mapping")
        target_id = _required_string(raw.get("id"), f"quality target {index}.id")
        if not _QUALITY_ID_RE.fullmatch(target_id):
            raise ValueError(f"invalid quality target id {target_id!r}")
        if target_id in ids:
            raise ValueError(f"duplicate quality target id {target_id!r}")
        source_group = _required_string(
            raw.get("source_group"), f"quality target {target_id}.source_group"
        )
        scope = _optional_string(raw.get("scope"))
        if scope and scope not in {"locked", "all"}:
            raise ValueError(f"quality target {target_id!r} has invalid scope")
        listener = raw.get("listener")
        if listener is None or (isinstance(listener, str) and not listener.strip()):
            if "port" not in raw:
                raise ValueError(f"quality target {target_id!r} listener is required")
            listener = f"http://127.0.0.1:{raw['port']}"
        elif not isinstance(listener, str):
            raise ValueError(f"quality target {target_id!r}.listener must be text")
        value = dict(raw)
        value.update(id=target_id, source_group=source_group, listener=listener)
        ids.add(target_id)
        result.append(value)
    return result


def _required_string(value: object, label: str) -> str:
    if not isinstance(value, str) or not value.strip():
        raise ValueError(f"{label} must be a non-empty string")
    return value.strip()


def _optional_string(value: object) -> str:
    if value is None:
        return ""
    if not isinstance(value, str):
        raise ValueError("quality provider must be a string")
    return value.strip()


def _string_sequence(value: object, label: str) -> list[str]:
    if value is None:
        return []
    values = value if isinstance(value, (list, tuple)) else [value]
    result = []
    for item in values:
        if not isinstance(item, str) or not item.strip():
            raise ValueError(f"{label} must contain only non-empty strings")
        result.append(item.strip())
    return result


_SAFE_PLAIN_SCALAR_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_.-]*$")


def _yaml_scalar(value: str) -> str:
    """Render a string as a safe YAML scalar for generated list entries."""

    if not isinstance(value, str):
        raise ValueError("generated YAML scalar must be a string")
    lowered = value.lower()
    if (
        _SAFE_PLAIN_SCALAR_RE.fullmatch(value)
        and lowered not in {"null", "true", "false", "yes", "no", "on", "off"}
        and not value.isdigit()
    ):
        return value
    return json.dumps(value, ensure_ascii=False)


def _listener_port(value: object, target_id: str) -> int:
    if not isinstance(value, str):
        raise ValueError(f"quality target {target_id!r}.listener must be an http URL")
    parsed = urlsplit(value.strip())
    if (
        parsed.scheme != "http"
        or parsed.hostname != "127.0.0.1"
        or parsed.username is not None
        or parsed.password is not None
        or parsed.path not in {"", "/"}
        or parsed.query
        or parsed.fragment
        or parsed.port is None
    ):
        raise ValueError(
            f"quality target {target_id!r}.listener must be an http loopback URL"
        )
    port = parsed.port
    if not 1 <= port <= 65535:
        raise ValueError(f"quality target {target_id!r}.listener port is invalid")
    return port


def _configured_mihomo_ports(text: str) -> set[int]:
    ports: set[int] = set()
    for line in text.splitlines():
        if not line or line[0].isspace() or line.lstrip().startswith("#"):
            continue
        match = re.match(r"^([A-Za-z0-9_-]+):\s*([^#\s]+)", line)
        if not match:
            continue
        key, value = match.groups()
        value = value.strip("'\"")
        if (key == "port" or key.endswith("-port")) and value.isdigit():
            port = int(value, 10)
            if port:
                ports.add(port)
        elif key == "external-controller":
            match_port = re.search(r":([0-9]+)$", value.strip("'\""))
            if match_port:
                ports.add(int(match_port.group(1), 10))
    return ports


def _section_bounds(
    lines: list[str], section: str
) -> tuple[int, int] | None:
    start = None
    for index, line in enumerate(lines):
        raw = line.rstrip("\r\n")
        if not raw or raw[0].isspace() or raw.lstrip().startswith("#"):
            continue
        if re.match(rf"^{re.escape(section)}:\s*(?:\[\s*\])?\s*(?:#.*)?$", raw):
            if start is not None:
                raise ValueError(f"duplicate top-level section {section!r}")
            start = index
    if start is None:
        return None
    end = len(lines)
    for index in range(start + 1, len(lines)):
        raw = lines[index].rstrip("\r\n")
        if raw and not raw[0].isspace() and not raw.startswith("#") and ":" in raw:
            end = index
            break
    return start, end


def _expand_empty_list_section(
    lines: list[str],
    bounds: tuple[int, int],
    section: str,
    newline: str,
) -> tuple[int, int]:
    """Turn ``section: []`` into a block header before inserting list items."""

    start, end = bounds
    raw = lines[start].rstrip("\r\n")
    match = re.fullmatch(
        rf"({re.escape(section)}:)\s*\[\s*\](\s*#.*)?", raw
    )
    if match is None:
        return bounds
    comment = match.group(2) or ""
    lines[start] = f"{match.group(1)}{comment}{newline}"
    return start, end


def _section_entries(
    lines: list[str], bounds: tuple[int, int], section: str
) -> tuple[list[dict[str, Any]], list[tuple[int, int]]]:
    start, end = bounds
    section_text = "".join(lines[start:end])
    parsed = _parse_yaml(section_text, f"mihomo {section}")
    if not isinstance(parsed, dict) or not isinstance(parsed.get(section), list):
        raise ValueError(f"mihomo {section} must be a list")
    entries = []
    for index, value in enumerate(parsed[section]):
        if not isinstance(value, dict):
            raise ValueError(f"mihomo {section}[{index}] must be a mapping")
        entries.append(value)
    item_starts = [
        index
        for index in range(start + 1, end)
        if _indent(lines[index]) == 2 and lines[index].lstrip().startswith("-")
    ]
    if len(item_starts) != len(entries):
        raise ValueError(f"mihomo {section} has unsupported list indentation")
    blocks = []
    for offset, item_start in enumerate(item_starts):
        block_end = item_starts[offset + 1] if offset + 1 < len(item_starts) else end
        # Keep section separator blank lines outside the final item.  This is
        # important for byte-stable idempotency when an owned final block is
        # replaced.
        if offset + 1 == len(item_starts):
            while block_end > item_start and not lines[block_end - 1].strip():
                block_end -= 1
        blocks.append((item_start, block_end))
    return entries, blocks


def _entry_names(entries: Sequence[Mapping[str, Any]], section: str) -> list[str]:
    names = []
    for index, entry in enumerate(entries):
        name = entry.get("name")
        if not isinstance(name, str) or not name.strip():
            raise ValueError(f"mihomo {section}[{index}].name is required")
        names.append(name.strip())
    if len(set(names)) != len(names):
        raise ValueError(f"duplicate {section} name")
    return names


def _owner_id(lines: list[str], block: tuple[int, int]) -> str | None:
    found = None
    for line in lines[block[0] : block[1]]:
        match = _QUALITY_OWNER_RE.match(line.rstrip("\r\n").strip())
        if match:
            if found is not None and found != match.group(1):
                raise ValueError("quality generated block has duplicate owners")
            found = match.group(1)
    return found


def _validate_owned_or_colliding(
    lines: list[str],
    blocks: Sequence[tuple[int, int]],
    names: Sequence[str],
    kind: str,
    wanted: set[str],
) -> None:
    prefix = _QUALITY_GROUP_PREFIX if kind == "group" else _QUALITY_LISTENER_PREFIX
    for index, name in enumerate(names):
        if not name.startswith(prefix):
            continue
        owner = _owner_id(lines, blocks[index])
        expected_id = name[len(prefix) :]
        if name in wanted and owner == expected_id:
            continue
        if owner is not None and owner == expected_id:
            # Stale guardian-owned blocks are safe to replace/remove.
            continue
        raise ValueError(f"quality {kind} name collision: {name!r}")


def _owned_indexes(
    lines: list[str],
    blocks: Sequence[tuple[int, int]],
    names: Sequence[str],
    prefix: str,
) -> list[int]:
    result = []
    for index, name in enumerate(names):
        owner = _owner_id(lines, blocks[index])
        if owner is not None and name == prefix + owner:
            result.append(index)
    return result


def _remove_blocks(
    lines: list[str],
    blocks: Sequence[tuple[int, int]],
    indexes: Sequence[int],
) -> list[str]:
    remove: set[int] = set()
    for index in indexes:
        start, end = blocks[index]
        # A comment/blank line after a generated item may belong to the user,
        # especially when the item is the last one in a section.  Keep those
        # lines outside the removal range while deleting the item itself.
        while end > start:
            stripped = lines[end - 1].strip()
            if stripped and not stripped.startswith("#"):
                break
            end -= 1
        remove.update(range(start, end))
    return [line for index, line in enumerate(lines) if index not in remove]


def _insert_section_blocks(
    lines: list[str],
    bounds: tuple[int, int],
    blocks: Sequence[str],
    newline: str,
) -> list[str]:
    if not blocks:
        return lines
    start, end = bounds
    insertion = end
    while insertion > start + 1 and not lines[insertion - 1].strip():
        insertion -= 1
    if insertion > 0 and not lines[insertion - 1].endswith(("\n", "\r")):
        lines[insertion - 1] += newline
    return lines[:insertion] + [block for block in blocks] + lines[insertion:]


def _append_new_section(
    lines: list[str], section: str, blocks: Sequence[str], newline: str
) -> list[str]:
    if lines and not lines[-1].endswith(("\n", "\r")):
        lines[-1] += newline
    if lines and lines[-1].strip():
        lines.append(newline)
    lines.append(f"{section}:{newline}")
    lines.extend(blocks)
    return lines


def _indent(line: str) -> int:
    return len(line) - len(line.lstrip(" ")) if line.strip() else 99


def _entry_port(entry: Mapping[str, Any], label: str) -> int:
    value = entry.get("port")
    if isinstance(value, bool):
        raise ValueError(f"{label}.port must be a decimal port")
    if isinstance(value, int):
        port = value
    elif isinstance(value, str) and value.strip().isdigit():
        port = int(value.strip(), 10)
    else:
        raise ValueError(f"{label}.port must be an explicit integer")
    if not 1 <= port <= 65535:
        raise ValueError(f"{label}.port is out of range")
    return port


def _validate_generated_quality_types(
    text: str, targets: Sequence[Mapping[str, object]]
) -> None:
    """Validate the final parsed quality groups, not only emitted text."""

    parsed = _parse_yaml(text, "patched mihomo config")
    if not isinstance(parsed, Mapping):
        raise ValueError("patched mihomo config must be a mapping")
    groups = parsed.get("proxy-groups")
    if not isinstance(groups, list):
        raise ValueError("patched mihomo proxy-groups must be a list")
    generated_names = {
        _QUALITY_GROUP_PREFIX + str(target["id"]) for target in targets
    }
    generated = {}
    for index, group in enumerate(groups):
        if not isinstance(group, Mapping):
            raise ValueError(f"patched mihomo proxy-groups[{index}] must be a mapping")
        name = group.get("name")
        if name in generated_names:
            generated[name] = group
        for key in ("use", "proxies"):
            if key not in group:
                continue
            values = group[key]
            if not isinstance(values, list) or not all(
                isinstance(value, str) for value in values
            ):
                raise ValueError(
                    f"patched mihomo proxy-groups[{index}].{key} must contain only strings"
                )
    for name in generated_names:
        group = generated.get(name)
        if group is None:
            raise ValueError(f"generated quality group {name!r} was not emitted")
        if not any(key in group for key in ("use", "proxies")):
            raise ValueError(f"generated quality group {name!r} has no node list")


def patch_file(
    path: str,
    main_group: str,
    backup_group: str,
    quality_targets: Sequence[Mapping[str, object]] = (),
) -> None:
    target = Path(path)
    with target.open("r", encoding="utf-8", newline="") as handle:
        original = handle.read()
    patched = patch_provider_groups(original, main_group, backup_group)
    if quality_targets:
        patched = patch_quality_targets(patched, quality_targets)
    temporary = target.with_name(target.name + ".guardian.tmp")
    with temporary.open("w", encoding="utf-8", newline="") as handle:
        handle.write(patched)
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

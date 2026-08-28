#!/usr/bin/env python3
"""Read-only discovery of a mihomo Compose deployment.

The module deliberately implements only the small YAML subset needed for
Compose and mihomo configuration files. It has no PyYAML dependency and
fails closed when the input is not unambiguous.
"""

from __future__ import annotations

import argparse
import ast
import dataclasses
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys
from typing import Any, Iterable, Mapping, Sequence
from urllib.parse import urlsplit


__all__ = [
    "Discovery",
    "discover_from_texts",
    "discover_quality_ports",
    "load_discovery",
    "prepare_quality_targets",
    "quality_targets_from_text",
    "read_proc_socket_ports",
    "render_guardian_config",
]


_TARGET = "mihomo-cliproxy"
_CONFIG_DEST = "/root/.config/mihomo/config.yaml"
_PROVIDERS_DEST = "/root/.config/mihomo/providers"
_SECRET_DESTINATIONS = frozenset(
    {
        "/root/.config/mihomo/.controller_secret",
        "/root/.config/mihomo/controller_secret",
        "/root/.config/mihomo/.secret",
        "/root/.config/mihomo/secret",
        "/guardian/controller_secret",
        "/guardian/secret",
    }
)
_SECRET_STORE: dict[int, str | None] = {}


class _YamlParser:
    """A strict, intentionally small YAML parser for the supported inputs."""

    def __init__(self, text: str):
        if not isinstance(text, str):
            raise ValueError("YAML input must be text")
        self.lines: list[tuple[int, int, str]] = []
        for number, raw in enumerate(text.splitlines(), 1):
            if "\t" in raw[: len(raw) - len(raw.lstrip())]:
                raise ValueError(f"YAML line {number}: tabs are not valid indentation")
            stripped = raw.lstrip(" ")
            if not stripped or stripped.startswith("#"):
                continue
            content = _strip_comment(stripped).rstrip()
            if not content or content in {"---", "..."}:
                continue
            indent = len(raw) - len(stripped)
            self.lines.append((number, indent, content))

    def parse(self) -> Any:
        if not self.lines:
            return {}
        value, index = self._block(0, self.lines[0][1])
        if index != len(self.lines):
            number, indent, text = self.lines[index]
            raise ValueError(
                f"YAML line {number}: unexpected indentation at {text!r}"
            )
        return value

    def _block(self, index: int, indent: int) -> tuple[Any, int]:
        if index >= len(self.lines):
            return {}, index
        number, actual_indent, text = self.lines[index]
        if actual_indent != indent:
            raise ValueError(
                f"YAML line {number}: expected indentation {indent}, got {actual_indent}"
            )
        if _is_list_item(text):
            return self._list(index, indent)
        return self._mapping(index, indent)

    def _mapping(self, index: int, indent: int) -> tuple[dict[str, Any], int]:
        result: dict[str, Any] = {}
        while index < len(self.lines):
            number, actual_indent, text = self.lines[index]
            if actual_indent < indent:
                break
            if actual_indent != indent:
                raise ValueError(
                    f"YAML line {number}: unexpected indentation {actual_indent}"
                )
            if _is_list_item(text):
                break
            key_text, value_text = _split_mapping(text, number)
            key_value = _parse_scalar(key_text, number)
            if not isinstance(key_value, str) or not key_value:
                raise ValueError(f"YAML line {number}: mapping key must be text")
            if key_value in result:
                raise ValueError(f"YAML line {number}: duplicate YAML key {key_value!r}")
            if value_text == "":
                if index + 1 < len(self.lines) and self.lines[index + 1][1] > indent:
                    value, index = self._block(index + 1, self.lines[index + 1][1])
                else:
                    value, index = {}, index + 1
            else:
                value = _parse_scalar(value_text, number)
                index += 1
                if index < len(self.lines) and self.lines[index][1] > indent:
                    child_number = self.lines[index][0]
                    raise ValueError(
                        f"YAML line {child_number}: nested value under scalar {key_value!r}"
                    )
            result[key_value] = value
        return result, index

    def _list(self, index: int, indent: int) -> tuple[list[Any], int]:
        result: list[Any] = []
        while index < len(self.lines):
            number, actual_indent, text = self.lines[index]
            if actual_indent < indent:
                break
            if actual_indent != indent or not _is_list_item(text):
                if actual_indent > indent:
                    raise ValueError(
                        f"YAML line {number}: list item indentation is ambiguous"
                    )
                break

            remainder = text[1:].lstrip(" ")
            index += 1
            if not remainder:
                if index < len(self.lines) and self.lines[index][1] > indent:
                    item, index = self._block(index, self.lines[index][1])
                else:
                    item = None
                result.append(item)
                continue

            if _has_mapping_separator(remainder):
                key_text, value_text = _split_mapping(remainder, number)
                key_value = _parse_scalar(key_text, number)
                if not isinstance(key_value, str) or not key_value:
                    raise ValueError(f"YAML line {number}: list mapping key must be text")
                item: dict[str, Any] = {}
                if value_text == "":
                    if index < len(self.lines) and self.lines[index][1] > indent:
                        value, index = self._block(index, self.lines[index][1])
                    else:
                        value, index = {}, index
                else:
                    value = _parse_scalar(value_text, number)
                item[key_value] = value

                while index < len(self.lines) and self.lines[index][1] > indent:
                    continuation_number, continuation_indent, continuation_text = (
                        self.lines[index]
                    )
                    if _is_list_item(continuation_text):
                        raise ValueError(
                            f"YAML line {continuation_number}: nested list mapping is ambiguous"
                        )
                    continuation_key, continuation_value = _split_mapping(
                        continuation_text, continuation_number
                    )
                    continuation_key_value = _parse_scalar(
                        continuation_key, continuation_number
                    )
                    if (
                        not isinstance(continuation_key_value, str)
                        or not continuation_key_value
                    ):
                        raise ValueError(
                            f"YAML line {continuation_number}: list mapping key must be text"
                        )
                    if continuation_key_value in item:
                        raise ValueError(
                            f"YAML line {continuation_number}: duplicate YAML key "
                            f"{continuation_key_value!r}"
                        )
                    index += 1
                    if continuation_value == "":
                        if (
                            index < len(self.lines)
                            and self.lines[index][1] > continuation_indent
                        ):
                            child, index = self._block(
                                index, self.lines[index][1]
                            )
                        else:
                            child = {}
                    else:
                        child = _parse_scalar(continuation_value, continuation_number)
                        if (
                            index < len(self.lines)
                            and self.lines[index][1] > continuation_indent
                        ):
                            child_number = self.lines[index][0]
                            raise ValueError(
                                f"YAML line {child_number}: nested value under scalar "
                                f"{continuation_key_value!r}"
                            )
                    item[continuation_key_value] = child
                result.append(item)
            else:
                item = _parse_scalar(remainder, number)
                if index < len(self.lines) and self.lines[index][1] > indent:
                    child_number = self.lines[index][0]
                    raise ValueError(
                        f"YAML line {child_number}: nested value under scalar list item"
                    )
                result.append(item)
        return result, index


def _strip_comment(text: str) -> str:
    quote: str | None = None
    escaped = False
    for index, char in enumerate(text):
        if quote == '"':
            if escaped:
                escaped = False
            elif char == "\\":
                escaped = True
            elif char == '"':
                quote = None
            continue
        if quote == "'":
            if char == "'":
                quote = None
            continue
        if char in {"'", '"'}:
            quote = char
        elif char == "#" and (index == 0 or text[index - 1].isspace()):
            return text[:index]
    return text


def _is_list_item(text: str) -> bool:
    return text == "-" or text.startswith("- ")


def _has_mapping_separator(text: str) -> bool:
    try:
        _split_mapping(text, 0)
    except ValueError:
        return False
    return True


def _split_mapping(text: str, number: int) -> tuple[str, str]:
    quote: str | None = None
    escaped = False
    depth = 0
    for index, char in enumerate(text):
        if quote == '"':
            if escaped:
                escaped = False
            elif char == "\\":
                escaped = True
            elif char == '"':
                quote = None
            continue
        if quote == "'":
            if char == "'":
                quote = None
            continue
        if char in {"'", '"'}:
            quote = char
            continue
        if char in "[{(":
            depth += 1
            continue
        if char in "]})":
            depth -= 1
            continue
        if char == ":" and depth == 0 and (
            index + 1 == len(text) or text[index + 1].isspace()
        ):
            return text[:index].strip(), text[index + 1 :].strip()
    raise ValueError(f"YAML line {number}: expected mapping key: value")


def _split_flow(text: str) -> list[str]:
    parts: list[str] = []
    start = 0
    quote: str | None = None
    escaped = False
    depth = 0
    for index, char in enumerate(text):
        if quote == '"':
            if escaped:
                escaped = False
            elif char == "\\":
                escaped = True
            elif char == '"':
                quote = None
            continue
        if quote == "'":
            if char == "'":
                quote = None
            continue
        if char in {"'", '"'}:
            quote = char
        elif char in "[{(":
            depth += 1
        elif char in "]})":
            depth -= 1
        elif char == "," and depth == 0:
            parts.append(text[start:index].strip())
            start = index + 1
    parts.append(text[start:].strip())
    return [part for part in parts if part]


def _parse_scalar(text: str, number: int) -> Any:
    text = text.strip()
    if text == "":
        return ""
    if text.startswith("[") and text.endswith("]"):
        inner = text[1:-1].strip()
        return [] if not inner else [_parse_scalar(part, number) for part in _split_flow(inner)]
    if text.startswith("{") and text.endswith("}"):
        inner = text[1:-1].strip()
        result: dict[str, Any] = {}
        if not inner:
            return result
        for part in _split_flow(inner):
            key_text, value_text = _split_mapping(part, number)
            key = _parse_scalar(key_text, number)
            if not isinstance(key, str):
                raise ValueError(f"YAML line {number}: flow mapping key must be text")
            if key in result:
                raise ValueError(f"YAML line {number}: duplicate YAML key {key!r}")
            result[key] = _parse_scalar(value_text, number)
        return result
    if text[0:1] in {"'", '"'} and text[-1:] == text[0]:
        try:
            if text[0] == '"':
                return json.loads(text)
            return ast.literal_eval(text)
        except (ValueError, SyntaxError, json.JSONDecodeError) as exc:
            raise ValueError(f"YAML line {number}: invalid quoted scalar") from exc
    lowered = text.lower()
    if lowered in {"null", "~"}:
        return None
    if lowered in {"true", "false"}:
        return lowered == "true"
    if re.fullmatch(r"-?[0-9]+", text):
        try:
            return int(text, 10)
        except ValueError:
            pass
    return text


def _parse_yaml(text: str, label: str) -> Any:
    try:
        return _YamlParser(text).parse()
    except ValueError as exc:
        raise ValueError(f"{label}: {exc}") from exc


def _mapping(value: Any, label: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise ValueError(f"{label} must be a YAML mapping")
    return value


def _string(value: Any, label: str) -> str:
    if not isinstance(value, str) or not value.strip():
        raise ValueError(f"{label} must be a non-empty string")
    return value.strip()


def _sequence(value: Any, label: str) -> list[Any]:
    if value is None:
        return []
    if isinstance(value, list):
        return value
    if isinstance(value, tuple):
        return list(value)
    if isinstance(value, str):
        return [value]
    raise ValueError(f"{label} must be a list or string")


def _normalise_name(value: Any, label: str) -> str:
    return _string(value, label)


def _normalise_inspect(inspect: Any) -> list[dict[str, Any]]:
    if inspect is None:
        return []
    if isinstance(inspect, (str, bytes, os.PathLike)):
        raw = Path(inspect).read_text() if isinstance(inspect, os.PathLike) else None
        if raw is None:
            raw_text = inspect.decode() if isinstance(inspect, bytes) else inspect
            try:
                value = json.loads(raw_text)
            except json.JSONDecodeError:
                possible_path = Path(raw_text)
                if not possible_path.is_file():
                    raise ValueError(
                        "inspect JSON must be valid JSON text or an existing file path"
                    )
                value = json.loads(possible_path.read_text())
        else:
            value = json.loads(raw)
    else:
        value = inspect
    if isinstance(value, dict):
        value = [value]
    if not isinstance(value, list) or not all(isinstance(item, dict) for item in value):
        raise ValueError("inspect JSON must be a Docker inspect object or list of objects")
    return value


def _labels(item: Mapping[str, Any]) -> dict[str, Any]:
    config = item.get("Config")
    if not isinstance(config, dict):
        config = {}
    labels = config.get("Labels")
    if not isinstance(labels, dict):
        labels = item.get("Labels")
    return labels if isinstance(labels, dict) else {}


def _inspect_image(item: Mapping[str, Any]) -> str:
    config = item.get("Config")
    if isinstance(config, dict) and isinstance(config.get("Image"), str):
        return config["Image"].lower()
    return str(item.get("Image", "")).lower()


def _inspect_name(item: Mapping[str, Any]) -> str:
    name = item.get("Name") or item.get("name") or item.get("ContainerName")
    return str(name or "").lstrip("/")


def _is_mihomo_identity(name: str, container: str, image: str, service: str) -> bool:
    return (
        name == _TARGET
        or container == _TARGET
        or service == _TARGET
        or "mihomo" in image
    )


@dataclasses.dataclass(frozen=True)
class _ComposeCandidate:
    service_name: str
    body: dict[str, Any]
    container_name: str
    image: str
    compose_labels: dict[str, str]


def _compose_candidates(compose: dict[str, Any]) -> list[_ComposeCandidate]:
    services = compose.get("services")
    services = _mapping(services, "Compose services")
    candidates: list[_ComposeCandidate] = []
    for service_name, raw_body in services.items():
        if not isinstance(service_name, str):
            raise ValueError("Compose service names must be strings")
        body = _mapping(raw_body, f"Compose service {service_name!r}")
        container_name = str(body.get("container_name") or "").lstrip("/")
        labels = body.get("labels")
        labels = _normalise_compose_labels(labels)
        image = str(body.get("image") or "").lower()
        service_label = str(labels.get("com.docker.compose.service") or "")
        if _is_mihomo_identity(
            service_name, container_name, image, service_label
        ):
            candidates.append(
                _ComposeCandidate(
                    service_name=service_name,
                    body=body,
                    container_name=container_name
                    or (_TARGET if service_name == _TARGET else service_name),
                    image=image,
                    compose_labels=labels,
                )
            )
    if not candidates:
        raise ValueError(
            "no unique mihomo-cliproxy Compose service candidate; "
            "name the service/container mihomo-cliproxy or use a mihomo image"
        )
    if len(candidates) != 1:
        names = ", ".join(candidate.service_name for candidate in candidates)
        raise ValueError(
            f"ambiguous mihomo-cliproxy Compose service candidates: {names}; "
            "refuse to guess"
        )
    return candidates


def _normalise_compose_labels(value: Any) -> dict[str, str]:
    if value is None:
        return {}
    if isinstance(value, dict):
        return {str(key): str(item) for key, item in value.items()}
    if isinstance(value, list):
        result: dict[str, str] = {}
        for item in value:
            if not isinstance(item, str) or "=" not in item:
                continue
            key, item_value = item.split("=", 1)
            result[key] = item_value
        return result
    return {}


def _inspect_candidates(items: list[dict[str, Any]]) -> list[dict[str, Any]]:
    candidates = []
    for item in items:
        labels = _labels(item)
        service = str(
            labels.get("com.docker.compose.service")
            or labels.get("com.docker.compose.service.name")
            or ""
        )
        name = _inspect_name(item)
        container = str(item.get("ContainerName") or "").lstrip("/")
        if _is_mihomo_identity(name, container, _inspect_image(item), service):
            candidates.append(item)
    return candidates


def _resolve_container(
    candidate: _ComposeCandidate, inspect_items: list[dict[str, Any]] | None
) -> dict[str, Any] | None:
    if inspect_items is None:
        return None
    candidates = _inspect_candidates(inspect_items)
    if not candidates:
        raise ValueError(
            "docker inspect contains no unique mihomo-cliproxy container candidate"
        )
    if len(candidates) != 1:
        names = ", ".join(_inspect_name(item) or "<unnamed>" for item in candidates)
        raise ValueError(
            f"ambiguous mihomo-cliproxy docker inspect candidates: {names}; "
            "refuse to guess"
        )
    item = candidates[0]
    labels = _labels(item)
    inspect_name = _inspect_name(item)
    if not inspect_name:
        raise ValueError(
            "docker inspect candidate is missing Name; refuse identity guessing"
        )
    if inspect_name != candidate.container_name:
        raise ValueError(
            "docker inspect Name does not match the Compose container_name; "
            "check --compose and --inspect-json"
        )

    inspect_service = labels.get("com.docker.compose.service")
    if not isinstance(inspect_service, str) or not inspect_service.strip():
        raise ValueError(
            "docker inspect candidate is missing the Compose service label; "
            "refuse identity guessing"
        )
    if inspect_service != candidate.service_name:
        raise ValueError(
            "docker inspect Compose service label does not match the Compose "
            "candidate service"
        )

    inspect_container = item.get("ContainerName")
    if inspect_container is not None:
        inspect_container = str(inspect_container).lstrip("/")
        if not inspect_container or inspect_container != candidate.container_name:
            raise ValueError(
                "docker inspect ContainerName does not match the Compose candidate"
            )

    inspect_image = _inspect_image(item)
    if not inspect_image:
        raise ValueError(
            "docker inspect candidate is missing Config.Image; refuse identity guessing"
        )
    if candidate.image and inspect_image != candidate.image:
        raise ValueError(
            "docker inspect Config.Image does not match the Compose candidate image"
        )
    for label, expected in candidate.compose_labels.items():
        if not label.startswith("com.docker.compose."):
            continue
        actual = labels.get(label)
        if actual != expected:
            raise ValueError(
                f"docker inspect Compose label {label!r} does not match the "
                "Compose candidate"
            )
    return item


@dataclasses.dataclass(frozen=True)
class _Mount:
    source: str | None
    destination: str


def _compose_mounts(body: Mapping[str, Any], cwd: Path | None) -> list[_Mount]:
    raw_mounts = body.get("volumes", [])
    mounts: list[_Mount] = []
    for raw in _sequence(raw_mounts, "Compose volumes"):
        if isinstance(raw, str):
            parts = raw.split(":")
            if len(parts) < 2:
                raise ValueError(
                    f"Compose volume {raw!r} must have source:destination syntax"
                )
            source, destination = parts[0], parts[1]
            if not destination.startswith("/"):
                raise ValueError(
                    f"Compose volume destination {destination!r} must be an absolute path"
                )
            mounts.append(
                _Mount(
                    source=_resolve_source(source, cwd),
                    destination=os.path.normpath(destination),
                )
            )
        elif isinstance(raw, dict):
            source = raw.get("source", raw.get("src"))
            destination = raw.get("target", raw.get("destination", raw.get("dst")))
            if destination is None:
                raise ValueError("Compose long volume syntax requires target")
            destination = _string(destination, "Compose volume target")
            if not destination.startswith("/"):
                raise ValueError(
                    f"Compose volume destination {destination!r} must be absolute"
                )
            mounts.append(
                _Mount(
                    source=_resolve_source(
                        str(source) if source is not None else "", cwd
                    ),
                    destination=os.path.normpath(destination),
                )
            )
        else:
            raise ValueError("Compose volume entries must be strings or mappings")
    return mounts


def _inspect_mounts(item: Mapping[str, Any]) -> list[_Mount]:
    raw_mounts = item.get("Mounts", [])
    if raw_mounts is None:
        return []
    if not isinstance(raw_mounts, list):
        raise ValueError("docker inspect Mounts must be a list")
    mounts: list[_Mount] = []
    for raw in raw_mounts:
        if not isinstance(raw, dict):
            raise ValueError("docker inspect Mounts entries must be objects")
        destination = raw.get("Destination", raw.get("destination"))
        if not isinstance(destination, str) or not destination.startswith("/"):
            continue
        source = raw.get("Source", raw.get("source"))
        mounts.append(
            _Mount(
                source=str(source) if isinstance(source, str) and source else None,
                destination=os.path.normpath(destination),
            )
        )
    return mounts


def _resolve_source(source: str, cwd: Path | None) -> str | None:
    source = source.strip()
    if not source or source.startswith("$" + "{"):
        return None
    if os.path.isabs(source):
        return os.path.normpath(source)
    if cwd is not None and (source.startswith(".") or "/" in source):
        return os.path.normpath(str(cwd / source))
    return None


def _merge_mounts(
    compose_mounts: Iterable[_Mount], inspect_mounts: Iterable[_Mount]
) -> list[_Mount]:
    merged: dict[str, _Mount] = {}
    for mount in (*compose_mounts, *inspect_mounts):
        previous = merged.get(mount.destination)
        if previous is not None and previous.source != mount.source:
            raise ValueError(
                f"mount destination {mount.destination!r} has conflicting sources; "
                "check Compose volumes and docker inspect"
            )
        if previous is None:
            merged[mount.destination] = mount
    return list(merged.values())


def _looks_like_secret(path: str | None) -> bool:
    return bool(path and os.path.normpath(path) in _SECRET_DESTINATIONS)


def _persistence_root(mount: _Mount) -> str | None:
    if not mount.source:
        return None
    destination = mount.destination
    if destination == "/guardian":
        return mount.source
    if not destination.startswith("/guardian/"):
        return None
    suffix = destination[len("/guardian/") :].split("/", 1)[0]
    if suffix in {"data", "logs", "run", "bin", "backups"}:
        return os.path.dirname(mount.source)
    return None


def _discover_paths(
    candidate: _ComposeCandidate,
    inspected: Mapping[str, Any] | None,
    cwd: Path | None,
    explicit_compose_path: str | None = None,
    explicit_config_path: str | None = None,
) -> dict[str, Any]:
    compose_mounts = _compose_mounts(candidate.body, cwd)
    inspect_mounts = _inspect_mounts(inspected) if inspected is not None else []
    mounts = _merge_mounts(compose_mounts, inspect_mounts)

    config_mounts = [
        mount
        for mount in mounts
        if mount.destination == _CONFIG_DEST
    ]
    provider_mounts = [
        mount
        for mount in mounts
        if mount.destination == _PROVIDERS_DEST
    ]
    secret_mounts = [
        mount
        for mount in mounts
        if _looks_like_secret(mount.destination) or _looks_like_secret(mount.source)
    ]
    secret_destinations = {mount.destination for mount in secret_mounts}
    if len(secret_destinations) > 1:
        values = ", ".join(sorted(secret_destinations))
        raise ValueError(
            f"ambiguous controller secret mounts ({values}); keep exactly one"
        )

    config_path = (
        explicit_config_path
        or (config_mounts[0].source if len(config_mounts) == 1 else None)
    )
    if len(config_mounts) > 1:
        sources = {mount.source for mount in config_mounts}
        if len(sources) > 1:
            raise ValueError("ambiguous mihomo config mounts; keep exactly one")
    providers_dir = (
        provider_mounts[0].source if len(provider_mounts) == 1 else None
    )
    if len(provider_mounts) > 1:
        sources = {mount.source for mount in provider_mounts}
        if len(sources) > 1:
            raise ValueError("ambiguous mihomo providers mounts; keep exactly one")

    labels = _labels(inspected or {})
    working_dir = labels.get("com.docker.compose.project.working_dir")
    compose_path = labels.get("com.docker.compose.project.config_files")
    if compose_path and not isinstance(compose_path, str):
        raise ValueError("Compose project config_files label must be text")
    if compose_path and "," in compose_path:
        raise ValueError(
            "Compose project has multiple config files; pass one --compose path explicitly"
        )
    if working_dir is not None and not isinstance(working_dir, str):
        raise ValueError("Compose working_dir label must be text")
    working_dir = os.path.normpath(working_dir) if working_dir else (
        str(cwd) if cwd is not None else None
    )
    if explicit_compose_path is not None:
        compose_path = explicit_compose_path
    elif compose_path:
        compose_path = os.path.normpath(compose_path)
    elif cwd is not None:
        possible = [
            cwd / name
            for name in ("docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml")
            if (cwd / name).is_file()
        ]
        if len(possible) == 1:
            compose_path = str(possible[0])
        elif len(possible) > 1:
            raise ValueError(
                "multiple Compose files found in cwd; pass one --compose path explicitly"
            )

    persistence = {
        root
        for root in (_persistence_root(mount) for mount in mounts)
        if root
    }
    secret_file = (
        next(iter(secret_destinations)) if len(secret_destinations) == 1 else None
    )
    host_secret_file = None
    if secret_mounts:
        secret_sources = {mount.source for mount in secret_mounts if mount.source}
        if len(secret_sources) > 1:
            raise ValueError(
                "ambiguous host controller secret files; keep exactly one"
            )
        host_secret_file = next(iter(secret_sources), None)

    return {
        "compose_path": compose_path,
        "compose_workdir": working_dir,
        "config_path": config_path,
        "providers_dir": providers_dir,
        "secret_file": secret_file,
        "host_secret_file": host_secret_file,
        "persistence_root_candidates": tuple(sorted(persistence)),
    }


def _port(value: Any, key: str) -> int | None:
    if value is None:
        return None
    if isinstance(value, bool):
        raise ValueError(f"{key} must be a decimal port integer, not boolean")
    if isinstance(value, int):
        port = value
    elif isinstance(value, str) and re.fullmatch(r"[0-9]+", value.strip()):
        port = int(value.strip(), 10)
    else:
        raise ValueError(
            f"{key} must be one unambiguous decimal port integer; "
            "environment substitutions and host:container mappings are unsupported"
        )
    if port == 0:
        return None
    if not 1 <= port <= 65535:
        raise ValueError(f"{key} must be between 1 and 65535")
    return port


def _controller_port(value: Any) -> int:
    controller = _string(value, "external-controller")
    if controller.startswith("http://") or controller.startswith("https://"):
        parsed = urlsplit(controller)
        if not parsed.hostname or parsed.port is None:
            raise ValueError(
                "external-controller must contain one explicit host:port"
            )
        port = parsed.port
    else:
        if controller.count(":") == 0:
            raise ValueError(
                "external-controller must contain one explicit host:port; "
                "a bare port is ambiguous"
            )
        if controller.startswith("["):
            match = re.fullmatch(r"\[[0-9A-Fa-f:.]+\]:(\d+)", controller)
            if not match:
                raise ValueError(
                    "external-controller IPv6 value must use [address]:port"
                )
            port = int(match.group(1), 10)
        else:
            host, separator, port_text = controller.rpartition(":")
            if (
                not separator
                or ":" in host
                or not re.fullmatch(r"[0-9]+", port_text)
            ):
                raise ValueError(
                    "external-controller must contain one explicit host:port "
                    "(host may be empty as :port)"
                )
            port = int(port_text, 10)
    if not 1 <= port <= 65535:
        raise ValueError("external-controller port must be between 1 and 65535")
    return port


_QUALITY_ID_RE = re.compile(r"^[a-z0-9][a-z0-9_-]{0,31}$")
_QUALITY_OWNER_RE = re.compile(
    r"^#\s*mihomo-guardian:\s*generated quality target\s+"
    r"([a-z0-9][a-z0-9_-]{0,31})\s*$"
)
_QUALITY_GROUP_PREFIX = "GUARDIAN-QUALITY-"
_QUALITY_LISTENER_PREFIX = "guardian-quality-"
_UNSET = object()


def quality_targets_from_text(text: str) -> list[dict[str, Any]]:
    """Read enabled quality targets from guardian.yaml using the narrow parser."""

    section = _quality_section_text(text, "quality")
    if section is None:
        return []
    quality = _mapping(_parse_yaml(section, "guardian quality"), "guardian quality")
    enabled = quality.get("enabled", False)
    if not isinstance(enabled, bool):
        raise ValueError("guardian quality.enabled must be boolean")
    if not enabled:
        return []
    raw_targets = quality.get("targets", [])
    if not isinstance(raw_targets, list) or not raw_targets:
        raise ValueError("enabled guardian quality requires targets")
    targets: list[dict[str, Any]] = []
    by_id: dict[str, dict[str, Any]] = {}
    for index, raw in enumerate(raw_targets):
        target = _mapping(raw, f"guardian quality.targets[{index}]")
        target_id = _string(target.get("id"), f"quality target {index}.id")
        if not _QUALITY_ID_RE.fullmatch(target_id):
            raise ValueError(f"invalid quality target id {target_id!r}")
        if target_id in by_id:
            raise ValueError(f"duplicate quality target id {target_id!r}")
        source_group = _string(
            target.get("source_group"),
            f"quality target {target_id}.source_group",
        )
        copied = dict(target)
        copied.update(id=target_id, source_group=source_group)
        by_id[target_id] = copied
        targets.append(copied)

    order = quality.get("order")
    if order is not None:
        if not isinstance(order, list) or len(order) != len(targets):
            raise ValueError("quality.order must list every target exactly once")
        ordered: list[dict[str, Any]] = []
        seen: set[str] = set()
        for value in order:
            target_id = _string(value, "quality.order entry")
            if target_id in seen or target_id not in by_id:
                raise ValueError("quality.order contains an unknown or duplicate target")
            seen.add(target_id)
            ordered.append(by_id[target_id])
        targets = ordered
    return targets


def discover_quality_ports(
    config_text: str,
    targets: Sequence[Mapping[str, object]],
    *,
    proc_tcp: str | None | object = _UNSET,
    proc_tcp6: str | None | object = _UNSET,
) -> list[int]:
    """Choose deterministic, container-local ports for quality listeners.

    Explicit target ports win.  A valid guardian-owned listener is reused when
    a target has no explicit port.  New ports start at 17990 and advance in
    order.  Both proc socket tables are required so an unavailable table can
    never turn a guessed port into a production config change.
    """

    config = _mapping(_parse_yaml(config_text, "mihomo config"), "mihomo config")
    configured_ports = _configured_ports(config)
    listeners = _quality_listener_records(config_text, config)
    listener_by_name: dict[str, dict[str, Any]] = {}
    listener_ports: dict[int, str] = {}
    user_listener_ports: set[int] = set()
    owned_listener_ports: set[int] = set()
    owned_listener_target_ports: dict[str, int] = {}
    owned_by_id: dict[str, dict[str, Any]] = {}
    for listener in listeners:
        name = listener["name"]
        if name in listener_by_name:
            raise ValueError(f"duplicate listener name {name!r}")
        listener_by_name[name] = listener
        port = listener["port"]
        previous = listener_ports.get(port)
        if previous is not None:
            raise ValueError(
                f"duplicate listener port {port} ({previous!r} and {name!r})"
            )
        listener_ports[port] = name
        owner = listener.get("owner")
        if owner is None:
            user_listener_ports.add(port)
            continue
        expected_name = _QUALITY_LISTENER_PREFIX + owner
        if name != expected_name:
            raise ValueError(
                f"owned listener {name!r} does not match target {owner!r}"
            )
        if (
            listener.get("type") != "mixed"
            or listener.get("listen") != "127.0.0.1"
            or listener.get("proxy") != _QUALITY_GROUP_PREFIX + owner
        ):
            raise ValueError(f"owned listener {name!r} is not a valid quality listener")
        owned_listener_ports.add(port)
        owned_by_id[owner] = listener
        owned_listener_target_ports[owner] = port

    bound_ports = read_proc_socket_ports(
        _socket_text(proc_tcp, "/proc/net/tcp", "tcp"),
        _socket_text(proc_tcp6, "/proc/net/tcp6", "tcp6"),
    )
    blocked = (
        configured_ports
        | user_listener_ports
        | bound_ports
    ) - owned_listener_ports

    normalised: list[tuple[str, dict[str, Any], int | None]] = []
    seen_ids: set[str] = set()
    target_ids = {
        raw.get("id")
        for raw in targets
        if isinstance(raw, Mapping) and isinstance(raw.get("id"), str)
    }
    used_ports: set[int] = set()
    for index, raw in enumerate(targets):
        target = _mapping(raw, f"quality target {index}")
        target_id = _string(target.get("id"), f"quality target {index}.id")
        if not _QUALITY_ID_RE.fullmatch(target_id):
            raise ValueError(f"invalid quality target id {target_id!r}")
        if target_id in seen_ids:
            raise ValueError(f"duplicate quality target id {target_id!r}")
        seen_ids.add(target_id)
        source_group = _string(
            target.get("source_group"), f"quality target {target_id}.source_group"
        )
        listener_name = _QUALITY_LISTENER_PREFIX + target_id
        existing = listener_by_name.get(listener_name)
        if existing is not None and existing.get("owner") != target_id:
            raise ValueError(f"quality listener name collision: {listener_name!r}")
        explicit = _quality_listener_port(target.get("listener"), target_id)
        if explicit is None and "port" in target:
            explicit = _quality_port_value(target.get("port"), target_id)
        port = explicit
        if port is None and existing is not None:
            port = existing["port"]
        if port is not None:
            if port in used_ports:
                raise ValueError(f"duplicate quality listener port {port}")
            if port in blocked and not (
                existing is not None and existing["port"] == port
            ):
                raise ValueError(f"quality listener port {port} is already in use")
            conflicting_owner = next(
                (
                    owner
                    for owner, owner_port in owned_listener_target_ports.items()
                    if owner_port == port and owner != target_id
                ),
                None,
            )
            if conflicting_owner is not None and conflicting_owner in target_ids:
                raise ValueError(
                    f"quality listener port {port} belongs to target {conflicting_owner!r}"
                )
            used_ports.add(port)
        normalised.append((target_id, {**target, "source_group": source_group}, port))

    next_port = 17990
    result: list[int] = []
    for target_id, target, port in normalised:
        if port is None:
            while next_port <= 65535 and (
                next_port in blocked or next_port in used_ports
            ):
                next_port += 1
            if next_port > 65535:
                raise ValueError("no safe quality listener port is available")
            port = next_port
            used_ports.add(port)
            next_port += 1
        result.append(port)
    return result


def prepare_quality_targets(
    config_text: str,
    targets: Sequence[Mapping[str, object]],
    *,
    proc_tcp: str | None | object = _UNSET,
    proc_tcp6: str | None | object = _UNSET,
) -> list[dict[str, Any]]:
    """Return target mappings with deterministic loopback listener URLs."""

    ports = discover_quality_ports(
        config_text, targets, proc_tcp=proc_tcp, proc_tcp6=proc_tcp6
    )
    prepared = []
    for raw, port in zip(targets, ports):
        target = dict(raw)
        target["listener"] = f"http://127.0.0.1:{port}"
        prepared.append(target)
    return prepared


def read_proc_socket_ports(tcp_text: str, tcp6_text: str) -> set[int]:
    """Parse Linux TCP socket tables; empty/unavailable input fails closed."""

    if not isinstance(tcp_text, str) or not tcp_text.strip():
        raise ValueError("tcp socket table is unavailable")
    if not isinstance(tcp6_text, str) or not tcp6_text.strip():
        raise ValueError("tcp6 socket table is unavailable")
    ports: set[int] = set()
    for table in (tcp_text, tcp6_text):
        for line in table.splitlines():
            fields = line.split()
            if not fields:
                continue
            first = fields[0].lower()
            if first == "sl":
                if len(fields) == 1 or fields[1].lower() == "local_address":
                    continue
                raise ValueError("tcp socket table has an unsupported header")
            if first == "local_address":
                if len(fields) == 1 or fields[1].lower() == "rem_address":
                    continue
                raise ValueError("tcp socket table has an unsupported header")
            if len(fields) < 2:
                raise ValueError("tcp socket table has an unsupported row")
            local_field = next(
                (
                    field
                    for field in fields[:3]
                    if re.fullmatch(r"[0-9A-Fa-f]+:[0-9A-Fa-f]{4}", field)
                ),
                None,
            )
            match = (
                re.fullmatch(r"[0-9A-Fa-f]+:([0-9A-Fa-f]{4})", local_field)
                if local_field is not None
                else None
            )
            if not match:
                raise ValueError("tcp socket table has an unsupported row")
            ports.add(int(match.group(1), 16))
    return ports


def _socket_text(value: str | None | object, path: str, label: str) -> str:
    if value is _UNSET:
        try:
            return Path(path).read_text(encoding="ascii")
        except OSError as exc:
            raise ValueError(f"{label} socket table is unavailable") from exc
    if value is None:
        raise ValueError(f"{label} socket table is unavailable")
    if not isinstance(value, str):
        raise ValueError(f"{label} socket table must be text")
    return value


def _quality_section_text(text: str, section: str) -> str | None:
    lines = text.splitlines(keepends=True)
    bounds = _section_bounds(lines, section)
    if bounds is None:
        return None
    # Return the section body, not its top-level key.  The caller for the
    # guardian config expects a mapping/list directly; retaining ``quality:``
    # would make enabled targets look like an empty disabled section.
    return "".join(lines[bounds[0] + 1 : bounds[1]])


def _configured_ports(config: Mapping[str, Any]) -> set[int]:
    ports: set[int] = set()
    for key, value in config.items():
        if key.endswith("-port"):
            port = _port(value, key)
            if port is not None:
                ports.add(port)
        elif key == "external-controller":
            ports.add(_controller_port(value))
    return ports


def _quality_listener_records(
    text: str, config: Mapping[str, Any]
) -> list[dict[str, Any]]:
    raw_listeners = config.get("listeners", [])
    if raw_listeners is None:
        return []
    if not isinstance(raw_listeners, list):
        raise ValueError("mihomo listeners must be a list")
    section = _quality_section_text(text, "listeners")
    if section is None:
        if raw_listeners:
            raise ValueError("mihomo listeners section could not be located")
        return []
    lines = section.splitlines(keepends=True)
    starts = [
        index
        for index, line in enumerate(lines)
        if _indent(line) == 2 and line.lstrip().startswith("-")
    ]
    if len(starts) != len(raw_listeners):
        raise ValueError("mihomo listeners use unsupported indentation")
    records = []
    for index, raw in enumerate(raw_listeners):
        listener = _mapping(raw, f"mihomo listeners[{index}]")
        name = _string(listener.get("name"), f"mihomo listeners[{index}].name")
        port = _quality_port_value(listener.get("port"), name)
        owner = None
        end = starts[index + 1] if index + 1 < len(starts) else len(lines)
        for line in lines[starts[index] : end]:
            match = _QUALITY_OWNER_RE.match(line.rstrip("\r\n").strip())
            if match:
                if owner is not None and owner != match.group(1):
                    raise ValueError("listener has duplicate quality owners")
                owner = match.group(1)
        records.append(
            {
                "name": name,
                "port": port,
                "type": listener.get("type"),
                "listen": listener.get("listen"),
                "proxy": listener.get("proxy"),
                "owner": owner,
            }
        )
    return records


def _quality_listener_port(value: object, target_id: str) -> int | None:
    if value is None or (isinstance(value, str) and not value.strip()):
        return None
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
    return _quality_port_value(parsed.port, target_id)


def _quality_port_value(value: object, label: str) -> int:
    if isinstance(value, bool):
        raise ValueError(f"{label} port must be an explicit integer")
    if isinstance(value, int):
        port = value
    elif isinstance(value, str) and value.strip().isdigit():
        port = int(value.strip(), 10)
    else:
        raise ValueError(f"{label} port must be an explicit integer")
    if not 1 <= port <= 65535:
        raise ValueError(f"{label} port is out of range")
    return port


def _indent(line: str) -> int:
    return len(line) - len(line.lstrip(" ")) if line.strip() else 99


def _group_data(config: Mapping[str, Any]) -> tuple[dict[str, str], dict[str, tuple[str, ...]]]:
    raw_groups = config.get("proxy-groups")
    if not isinstance(raw_groups, list) or not raw_groups:
        raise ValueError("mihomo proxy-groups must be a non-empty list")
    raw_providers = config.get("proxy-providers")
    if raw_providers is None:
        declared_providers: dict[str, Any] = {}
    else:
        declared_providers = _mapping(
            raw_providers, "mihomo proxy-providers"
        )

    groups: dict[str, dict[str, Any]] = {}
    for index, raw_group in enumerate(raw_groups):
        group = _mapping(raw_group, f"proxy-groups[{index}]")
        name = _normalise_name(group.get("name"), f"proxy-groups[{index}].name")
        if name in groups:
            raise ValueError(f"duplicate proxy group name {name!r}")
        groups[name] = group

    provider_map: dict[str, tuple[str, ...]] = {}
    for name, group in groups.items():
        if "use" not in group:
            continue
        if group.get("use") in (None, {}, [], ""):
            raise ValueError(
                f"group {name!r}.use cannot be empty; declare provider names"
            )
        providers: list[str] = []
        for index, value in enumerate(_sequence(group.get("use"), f"group {name!r}.use")):
            provider = _normalise_name(value, f"group {name!r}.use[{index}]")
            if provider in providers:
                raise ValueError(
                    f"duplicate provider {provider!r} in group {name!r}.use"
                )
            if provider not in declared_providers:
                raise ValueError(
                    f"group {name!r}.use references undeclared provider {provider!r}; "
                    "add it under top-level proxy-providers"
                )
            providers.append(provider)
        provider_map[name] = tuple(providers)

    structural_channels: list[tuple[str, list[str]]] = []
    for name, group in groups.items():
        group_type = str(group.get("type") or "").lower()
        if group_type != "select":
            continue
        raw_proxies = _sequence(group.get("proxies"), f"group {name!r}.proxies")
        if len(raw_proxies) != 2:
            continue
        references = []
        for value in raw_proxies:
            proxy_name = _normalise_name(value, f"group {name!r}.proxies entry")
            if proxy_name in references:
                references = []
                break
            references.append(proxy_name)
        if len(references) == 2 and all(reference in groups for reference in references):
            structural_channels.append((name, references))

    if len(structural_channels) != 1:
        if not structural_channels:
            raise ValueError(
                "could not find one select proxy group whose proxies reference "
                "exactly two main/backup groups"
            )
        names = ", ".join(name for name, _ in structural_channels)
        raise ValueError(
            f"ambiguous channel group relationships ({names}); "
            "exactly one channel relationship is required"
        )

    channel_name, referenced = structural_channels[0]
    main_matches = [
        name for name in referenced if _role_matches(name, "main")
    ]
    backup_matches = [
        name for name in referenced if _role_matches(name, "backup")
    ]
    if len(main_matches) != 1 or len(backup_matches) != 1:
        provider_roles = {
            name: provider_map.get(name, ())
            for name in referenced
        }
        main_matches = [
            name for name in referenced
            if any(_role_matches(provider, "main") for provider in provider_roles[name])
        ]
        backup_matches = [
            name for name in referenced
            if any(_role_matches(provider, "backup") for provider in provider_roles[name])
        ]
    if len(main_matches) != 1 or len(backup_matches) != 1:
        raise ValueError(
            f"cannot classify channel {channel_name!r} references {referenced!r} "
            "as one main and one backup group; rename groups or providers"
        )
    if main_matches[0] == backup_matches[0]:
        raise ValueError(
            f"channel {channel_name!r} has ambiguous main/backup relationship"
        )

    return (
        {"channel": channel_name, "main": main_matches[0], "backup": backup_matches[0]},
        provider_map,
    )


def _role_matches(value: str, role: str) -> bool:
    lowered = value.lower().replace("_", "-")
    if role == "main":
        return (
            lowered in {"main", "primary", "primary-channel"}
            or bool(re.search(r"(^|-)main($|-)", lowered))
            or "primary" in lowered
        )
    return (
        lowered in {"backup", "standby", "secondary", "failover"}
        or bool(re.search(r"(^|-)backup($|-)", lowered))
        or any(token in lowered for token in ("standby", "secondary", "failover"))
    )


@dataclasses.dataclass(frozen=True, slots=True)
class Discovery:
    service_name: str
    container_name: str
    compose_path: str | None
    compose_workdir: str | None
    config_path: str | None
    providers_dir: str | None
    secret_file: str | None
    host_secret_file: str | None
    persistence_root_candidates: tuple[str, ...]
    api: str
    proxy: str
    mixed_port: int | None
    http_port: int | None
    socks_port: int | None
    secret_configured: bool
    groups: dict[str, str]
    providers: dict[str, tuple[str, ...]]

    @property
    def secret(self) -> str | None:
        """Return the secret only to in-process callers; never serialize it."""

        return _SECRET_STORE.get(id(self))

    def __del__(self) -> None:
        store = globals().get("_SECRET_STORE")
        if store is not None:
            store.pop(id(self), None)

    @property
    def has_secret(self) -> bool:
        return self.secret_configured

    @property
    def host_config_path(self) -> str | None:
        return self.config_path

    @property
    def mihomo_config_path(self) -> str | None:
        return self.config_path

    @property
    def provider_dir(self) -> str | None:
        return self.providers_dir

    @property
    def persistence_roots(self) -> tuple[str, ...]:
        return self.persistence_root_candidates

    def public_dict(self) -> dict[str, Any]:
        return {
            "service_name": self.service_name,
            "container_name": self.container_name,
            "compose_path": self.compose_path,
            "compose_workdir": self.compose_workdir,
            "config_path": self.config_path,
            "providers_dir": self.providers_dir,
            "secret_file": self.secret_file,
            "host_secret_file": self.host_secret_file,
            "persistence_root_candidates": list(self.persistence_root_candidates),
            "api": self.api,
            "proxy": self.proxy,
            "mixed_port": self.mixed_port,
            "http_port": self.http_port,
            "socks_port": self.socks_port,
            "has_secret": self.secret_configured,
            "groups": dict(self.groups),
            "providers": {
                group: list(names) for group, names in self.providers.items()
            },
        }


def discover_from_texts(
    compose_text: str,
    config_text: str,
    inspect: Any = None,
    cwd: str | os.PathLike[str] | None = None,
) -> Discovery:
    """Discover one deployment without touching Docker or writing files."""

    compose = _mapping(_parse_yaml(compose_text, "Compose"), "Compose")
    config = _mapping(_parse_yaml(config_text, "mihomo config"), "mihomo config")
    compose_candidates = _compose_candidates(compose)
    candidate = compose_candidates[0]
    inspect_items = None if inspect is None else _normalise_inspect(inspect)
    inspected = _resolve_container(candidate, inspect_items)

    cwd_path = Path(cwd).resolve() if cwd is not None else None
    paths = _discover_paths(candidate, inspected, cwd_path)

    mixed_port = _port(config.get("mixed-port"), "mixed-port")
    http_port = _port(config.get("http-port"), "http-port")
    socks_port = _port(config.get("socks-port"), "socks-port")
    controller_port = _controller_port(config.get("external-controller"))

    if mixed_port is not None:
        proxy = f"http://127.0.0.1:{mixed_port}"
    elif http_port is not None:
        proxy = f"http://127.0.0.1:{http_port}"
    elif socks_port is not None:
        proxy = f"socks5://127.0.0.1:{socks_port}"
    else:
        raise ValueError(
            "mihomo must expose a usable mixed-port, http-port, or socks-port"
        )

    secret_value = config.get("secret")
    if secret_value is None:
        secret_configured = False
    elif isinstance(secret_value, str):
        secret_configured = bool(secret_value.strip())
    else:
        raise ValueError("mihomo secret must be a scalar string when configured")

    groups, providers = _group_data(config)
    discovery = Discovery(
        service_name=candidate.service_name,
        container_name=candidate.container_name,
        api=f"http://127.0.0.1:{controller_port}",
        proxy=proxy,
        mixed_port=mixed_port,
        http_port=http_port,
        socks_port=socks_port,
        secret_configured=secret_configured,
        groups=groups,
        providers=providers,
        **paths,
    )
    _SECRET_STORE[id(discovery)] = (
        secret_value if isinstance(secret_value, str) else None
    )
    return discovery


def load_discovery(
    compose_path: str | os.PathLike[str],
    config_path: str | os.PathLike[str],
    inspect_json: Any = None,
) -> Discovery:
    """Load the three read-only inputs and return their discovery result."""

    compose = Path(compose_path).resolve()
    config = Path(config_path).resolve()
    compose_text = compose.read_text()
    config_text = config.read_text()
    inspect = inspect_json
    if isinstance(inspect_json, (str, os.PathLike, bytes)):
        if isinstance(inspect_json, os.PathLike):
            inspect = Path(inspect_json).read_text()
        elif isinstance(inspect_json, bytes):
            inspect = inspect_json.decode()
        else:
            possible_path = Path(inspect_json)
            if possible_path.is_file():
                inspect = possible_path.read_text()
    discovery = discover_from_texts(
        compose_text,
        config_text,
        inspect=inspect,
        cwd=compose.parent,
    )
    updated = dataclasses.replace(
        discovery,
        compose_path=str(compose),
        config_path=str(config),
        compose_workdir=str(compose.parent),
    )
    _SECRET_STORE[id(updated)] = discovery.secret
    return updated


def _top_level_key(line: str) -> str | None:
    if not line or line[0].isspace() or line.lstrip().startswith("#"):
        return None
    try:
        key, value = _split_mapping(_strip_comment(line.rstrip()), 0)
    except ValueError:
        return None
    if value.startswith("|") or value.startswith(">"):
        return None
    parsed = _parse_scalar(key, 0)
    return parsed if isinstance(parsed, str) else None


def _section_bounds(lines: list[str], section: str) -> tuple[int, int] | None:
    start = None
    for index, line in enumerate(lines):
        if _top_level_key(line) == section:
            if start is not None:
                raise ValueError(f"template has duplicate top-level section {section!r}")
            start = index
    if start is None:
        return None
    end = len(lines)
    for index in range(start + 1, len(lines)):
        if _top_level_key(lines[index]) is not None:
            end = index
            break
    return start, end


def _direct_key(line: str, section_indent: int) -> str | None:
    if not line.strip() or line.lstrip().startswith("#"):
        return None
    indent = len(line) - len(line.lstrip(" "))
    if indent <= section_indent:
        return None
    if indent != section_indent + 2:
        return None
    try:
        key, _ = _split_mapping(_strip_comment(line.strip()), 0)
    except ValueError:
        return None
    parsed = _parse_scalar(key, 0)
    return parsed if isinstance(parsed, str) else None


def _provider_value(names: Sequence[str]) -> str:
    if len(names) == 1:
        return names[0]
    return "[" + ", ".join(names) + "]"


def _render_section(
    lines: list[str],
    section: str,
    updates: Mapping[str, str],
    add_if_missing: bool = True,
    deletes: Iterable[str] = (),
) -> list[str]:
    bounds = _section_bounds(lines, section)
    if bounds is None:
        if not updates or not add_if_missing:
            return lines
        if lines and lines[-1].strip():
            lines.append("\n")
        lines.append(f"{section}:\n")
        lines.extend(f"  {key}: {value}\n" for key, value in updates.items())
        return lines

    start, end = bounds
    section_line = lines[start]
    section_indent = len(section_line) - len(section_line.lstrip(" "))
    delete_keys = set(deletes)
    delete_indexes: list[int] = []
    seen_deleted: set[str] = set()
    for index in range(start + 1, end):
        key = _direct_key(lines[index], section_indent)
        if key not in delete_keys:
            continue
        if key in seen_deleted:
            raise ValueError(
                f"template section {section!r} has duplicate infrastructure key {key!r}"
            )
        delete_indexes.append(index)
        seen_deleted.add(key)
    for index in reversed(delete_indexes):
        del lines[index]

    bounds = _section_bounds(lines, section)
    if bounds is None:
        return lines
    start, end = bounds
    seen: set[str] = set()
    for index in range(start + 1, end):
        key = _direct_key(lines[index], section_indent)
        if key not in updates:
            continue
        if key in seen:
            raise ValueError(
                f"template section {section!r} has duplicate infrastructure key {key!r}"
            )
        newline = "\n" if lines[index].endswith("\n") else ""
        prefix = lines[index][: len(lines[index]) - len(lines[index].lstrip(" "))]
        lines[index] = f"{prefix}{key}: {updates[key]}{newline}"
        seen.add(key)

    if add_if_missing:
        missing = [key for key in updates if key not in seen]
        if missing:
            insertion = end
            while insertion > start + 1 and not lines[insertion - 1].strip():
                insertion -= 1
            newline = "\n" if any(line.endswith("\n") for line in lines) else ""
            lines[insertion:insertion] = [
                f"{' ' * (section_indent + 2)}{key}: {updates[key]}{newline}"
                for key in missing
            ]
    return lines


def render_guardian_config(template_text: str, discovery: Discovery) -> str:
    """Render only the infrastructure portions of a guardian config template."""

    if not isinstance(discovery, Discovery):
        raise TypeError("discovery must be a Discovery instance")
    lines = template_text.splitlines(keepends=True)
    lines = _render_section(
        lines,
        "mihomo",
        {
            "api": discovery.api,
            "proxy": discovery.proxy,
            **({"secret_file": discovery.secret_file} if discovery.secret_file else {}),
        },
        deletes=("secret_file",) if discovery.secret_file is None else (),
    )
    lines = _render_section(
        lines,
        "groups",
        {
            "channel": discovery.groups["channel"],
            "main": discovery.groups["main"],
            "backup": discovery.groups["backup"],
        },
    )
    main_providers = discovery.providers.get(discovery.groups["main"], ())
    backup_providers = discovery.providers.get(discovery.groups["backup"], ())
    lines = _render_section(
        lines,
        "providers",
        {
            "main": _provider_value(main_providers),
            "backup": _provider_value(backup_providers),
        },
    )
    return "".join(lines)


def _env_value(value: Any) -> str:
    if value is None:
        return ""
    return shlex.quote(str(value))


def _cli_env(discovery: Discovery) -> str:
    public = discovery.public_dict()
    values = {
        "MIHOMO_SERVICE": public["service_name"],
        "MIHOMO_CONTAINER": public["container_name"],
        "MIHOMO_COMPOSE": public["compose_path"],
        "MIHOMO_COMPOSE_WORKDIR": public["compose_workdir"],
        "MIHOMO_CONFIG": public["config_path"],
        "MIHOMO_PROVIDERS_DIR": public["providers_dir"],
        "MIHOMO_SECRET_FILE": public["secret_file"],
        "MIHOMO_HAS_SECRET": "1" if public["has_secret"] else "0",
        "MIHOMO_API": public["api"],
        "MIHOMO_PROXY": public["proxy"],
        "MIHOMO_MIXED_PORT": public["mixed_port"],
        "MIHOMO_HTTP_PORT": public["http_port"],
        "MIHOMO_SOCKS_PORT": public["socks_port"],
        "MIHOMO_CHANNEL_GROUP": public["groups"]["channel"],
        "MIHOMO_MAIN_GROUP": public["groups"]["main"],
        "MIHOMO_BACKUP_GROUP": public["groups"]["backup"],
        "MIHOMO_MAIN_PROVIDERS": ",".join(
            public["providers"].get(public["groups"]["main"], [])
        ),
        "MIHOMO_BACKUP_PROVIDERS": ",".join(
            public["providers"].get(public["groups"]["backup"], [])
        ),
    }
    return "".join(f"{key}={_env_value(value)}\n" for key, value in values.items())


def _docker_output(args: Sequence[str]) -> str:
    try:
        result = subprocess.run(
            list(args), capture_output=True, text=True, check=False
        )
    except OSError as exc:
        raise ValueError(f"cannot execute Docker for automatic discovery: {exc}") from exc
    if result.returncode != 0:
        detail = (result.stderr or result.stdout or "").strip()
        suffix = f": {detail}" if detail else ""
        raise ValueError(
            f"Docker command {' '.join(args)!r} failed with exit status "
            f"{result.returncode}{suffix}"
        )
    return result.stdout


def _unique_existing_paths(paths: Iterable[Path], label: str) -> Path:
    unique: dict[str, Path] = {}
    for path in paths:
        resolved = path.resolve()
        if resolved.is_file():
            unique[str(resolved)] = resolved
    if len(unique) != 1:
        candidates = ", ".join(sorted(unique)) or "none"
        raise ValueError(
            f"automatic discovery needs exactly one {label}; found {candidates}"
        )
    return next(iter(unique.values()))


def _auto_config_candidates(cwd: Path, inspected: Mapping[str, Any]) -> list[Path]:
    candidates: list[Path] = []
    for mount in inspected.get("Mounts", []) or []:
        if not isinstance(mount, Mapping):
            continue
        if mount.get("Destination") == _CONFIG_DEST:
            source = mount.get("Source")
            if isinstance(source, str) and source:
                candidates.append(Path(source))
    candidates.extend(
        cwd / name
        for name in (
            "config/config.yaml",
            "config/config.yml",
            "config.yaml",
            "config.yml",
        )
    )
    direct = {path.resolve() for path in candidates}
    if not any(path.is_file() for path in direct):
        for path in cwd.rglob("config.yaml"):
            if any(part in {".git", "node_modules"} for part in path.parts):
                continue
            candidates.append(path)
    return candidates


def _auto_inputs(
    *,
    cwd: Path,
    container: str | None,
    compose_path: str | None,
    config_path: str | None,
    inspect_json: Any,
) -> tuple[Path, Path, Any]:
    if not cwd.is_dir():
        raise ValueError(f"automatic discovery cwd is not a directory: {cwd}")

    if inspect_json is None:
        selected_container = container
        if not selected_container:
            names = [
                line.strip()
                for line in _docker_output(
                    ["docker", "ps", "-a", "--format", "{{.Names}}"]
                ).splitlines()
                if line.strip()
            ]
            exact = [name for name in names if name == _TARGET]
            if len(exact) == 1:
                selected_container = exact[0]
            else:
                candidates = [name for name in names if "mihomo" in name.lower()]
                if len(candidates) != 1:
                    found = ", ".join(candidates) or "none"
                    raise ValueError(
                        "automatic discovery found no unique mihomo container "
                        f"(candidates: {found}); pass --container"
                    )
                selected_container = candidates[0]
        inspect_text = _docker_output(["docker", "inspect", selected_container])
        inspect = _normalise_inspect(inspect_text)
    else:
        inspect = _normalise_inspect(inspect_json)

    if len(inspect) != 1:
        raise ValueError(
            "automatic discovery requires docker inspect to return exactly one container"
        )
    inspected = inspect[0]
    labels = _labels(inspected)

    if compose_path:
        compose = Path(compose_path)
        if not compose.is_file():
            raise ValueError(f"Compose file not found: {compose}")
        compose = compose.resolve()
    else:
        label_value = labels.get("com.docker.compose.project.config_files")
        label_candidates: list[Path] = []
        if isinstance(label_value, str) and label_value.strip():
            if "," in label_value:
                raise ValueError(
                    "automatic discovery found multiple Compose files in Docker labels; "
                    "pass --compose"
                )
            label_candidates.append(Path(label_value.strip()))
        compose_candidates = label_candidates + [
            cwd / name
            for name in ("docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml")
        ]
        compose = _unique_existing_paths(compose_candidates, "Compose file")

    if config_path:
        config = Path(config_path)
        if not config.is_file():
            raise ValueError(f"mihomo config file not found: {config}")
        config = config.resolve()
    else:
        config = _unique_existing_paths(
            _auto_config_candidates(cwd, inspected), "mihomo config file"
        )
    return compose, config, inspect


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Read-only mihomo-cliproxy infrastructure discovery"
    )
    parser.add_argument("--auto", action="store_true", help="discover Docker inputs automatically")
    parser.add_argument("--cwd", default=".", help="deployment directory used by --auto")
    parser.add_argument("--container", help=f"container name used by --auto (default: {_TARGET})")
    parser.add_argument("--compose", help="Compose YAML path")
    parser.add_argument("--config", help="mihomo config.yaml path")
    parser.add_argument("--inspect-json", help="docker inspect JSON path")
    parser.add_argument("--format", choices=("json", "env"), default="json")
    args = parser.parse_args(argv)
    try:
        if args.auto:
            compose, config, inspect = _auto_inputs(
                cwd=Path(args.cwd).resolve(),
                container=args.container,
                compose_path=args.compose,
                config_path=args.config,
                inspect_json=args.inspect_json,
            )
            discovery = load_discovery(compose, config, inspect)
        else:
            if not args.compose or not args.config:
                parser.error("--compose and --config are required unless --auto is used")
            discovery = load_discovery(args.compose, args.config, args.inspect_json)
        if args.format == "json":
            sys.stdout.write(
                json.dumps(
                    discovery.public_dict(),
                    ensure_ascii=False,
                    sort_keys=True,
                    indent=2,
                )
                + "\n"
            )
        else:
            sys.stdout.write(_cli_env(discovery))
        return 0
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        sys.stderr.write(f"discover: {exc}\n")
        return 2


if __name__ == "__main__":
    raise SystemExit(main())

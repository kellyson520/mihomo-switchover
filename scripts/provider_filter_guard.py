#!/usr/bin/env python3
"""Fail-closed validation for mihomo provider filters.

The guard checks only cached proxy names. It deliberately does not rewrite
filters or expose provider URLs and proxy definitions.
"""

from __future__ import annotations

from dataclasses import dataclass
import re
from pathlib import Path
from typing import Any, Mapping, Sequence

from scripts.discover import _mapping, _parse_yaml, _sequence


class ProviderFilterError(ValueError):
    """A configured provider filter cannot be proven to retain a node."""


@dataclass(frozen=True)
class ProviderFilterReport:
    provider: str
    cache_count: int
    match_count: int


def validate_nonempty_provider_filters(
    config_text: str,
    config_path: str | Path,
    providers_dir: str | Path | None,
    provider_names: Sequence[str],
) -> list[ProviderFilterReport]:
    """Validate every selected provider that declares a non-empty filter.

    Unfiltered providers are intentionally accepted without a cache because
    a cache is not required to prove that an unrestricted provider is safe.
    Filtered providers require a readable cache and at least one matching
    ``proxies[].name`` value.
    """

    parsed = _parse_yaml(config_text, "mihomo config")
    root = _mapping(parsed, "mihomo config")
    declared = _mapping(root.get("proxy-providers", {}), "mihomo proxy-providers")
    config_file = Path(config_path)
    cache_root = Path(providers_dir) if providers_dir is not None else None
    reports: list[ProviderFilterReport] = []
    for raw_name in provider_names:
        name = str(raw_name).strip()
        if not name:
            continue
        if name not in declared:
            raise ProviderFilterError(f"provider {name!r} is not declared")
        provider = _mapping(declared[name], f"provider {name!r}")
        filter_value = provider.get("filter")
        if filter_value in (None, ""):
            continue
        if not isinstance(filter_value, str) or not filter_value.strip():
            raise ProviderFilterError(f"provider {name!r} has an invalid filter")
        # The repository's small YAML parser uses Python literal parsing for
        # single-quoted scalars; restore the YAML-preserved ``\\b`` escape if
        # it was decoded to a backspace by that compatibility parser.
        filter_pattern = filter_value.replace(chr(8), r"\b")
        try:
            matcher = re.compile(filter_pattern)
        except re.error as exc:
            raise ProviderFilterError(f"provider {name!r} has an invalid filter regex") from exc
        cache_path = _provider_cache_path(provider, config_file, cache_root)
        if cache_path is None or not cache_path.is_file():
            raise ProviderFilterError(
                f"provider {name!r} filter cannot be validated: cache unavailable"
            )
        try:
            cache = _parse_yaml(cache_path.read_text(encoding="utf-8"), f"provider {name!r} cache")
            cache_mapping = _mapping(cache, f"provider {name!r} cache")
            raw_proxies = cache_mapping.get("proxies", [])
            proxies = _sequence(raw_proxies, f"provider {name!r} cache proxies")
        except (OSError, ValueError) as exc:
            raise ProviderFilterError(
                f"provider {name!r} filter cannot be validated: cache unreadable"
            ) from exc
        names: list[str] = []
        for item in proxies:
            if isinstance(item, Mapping) and isinstance(item.get("name"), str):
                names.append(item["name"])
        match_count = sum(1 for node_name in names if matcher.search(node_name))
        if match_count == 0:
            raise ProviderFilterError(
                f"provider {name!r} filter has no cached matches "
                f"(cache_count={len(names)}, match_count=0)"
            )
        reports.append(
            ProviderFilterReport(
                provider=name, cache_count=len(names), match_count=match_count
            )
        )
    return reports


def _provider_cache_path(
    provider: Mapping[str, Any], config_path: Path, providers_dir: Path | None
) -> Path | None:
    raw_path = provider.get("path")
    if not isinstance(raw_path, str) or not raw_path.strip():
        return None
    declared_path = Path(raw_path)
    candidates: list[Path] = []
    if declared_path.is_absolute():
        candidates.append(declared_path)
    else:
        candidates.append(config_path.parent / declared_path)
        if providers_dir is not None:
            candidates.append(providers_dir / declared_path)
            candidates.append(providers_dir / declared_path.name)
    for candidate in candidates:
        if candidate.is_file():
            return candidate
    return candidates[0] if candidates else None

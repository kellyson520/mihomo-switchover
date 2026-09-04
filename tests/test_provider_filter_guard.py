import pytest

from scripts.provider_filter_guard import (
    ProviderFilterError,
    validate_nonempty_provider_filters,
)


CONFIG = """proxy-providers:
  backup:
    type: http
    path: ./backup.yaml
    filter: '(美国|🇺🇸|\\bUS\\b|United States)'
  unfiltered:
    type: http
    path: ./unfiltered.yaml
"""


def write_provider(path, names):
    path.write_text(
        "proxies:\n" + "".join(f"  - name: {name}\n    type: vmess\n" for name in names),
        encoding="utf-8",
    )


def test_accepts_compatible_region_names_and_reports_counts(tmp_path):
    config_path = tmp_path / "config.yaml"
    config_path.write_text(CONFIG, encoding="utf-8")
    write_provider(
        tmp_path / "backup.yaml",
        ["🇺🇸 Los Angeles", "美国-01", "US New York", "United States West"],
    )

    reports = validate_nonempty_provider_filters(
        config_path.read_text(encoding="utf-8"), config_path, tmp_path, ["backup"]
    )

    assert len(reports) == 1
    assert reports[0].cache_count == 4
    assert reports[0].match_count == 4


def test_rejects_zero_match_without_exposing_provider_contents(tmp_path):
    config_path = tmp_path / "config.yaml"
    config_path.write_text(CONFIG, encoding="utf-8")
    write_provider(tmp_path / "backup.yaml", ["Hong Kong secret-token"])

    with pytest.raises(ProviderFilterError) as caught:
        validate_nonempty_provider_filters(
            config_path.read_text(encoding="utf-8"), config_path, tmp_path, ["backup"]
        )

    message = str(caught.value)
    assert "backup" in message
    assert "match_count=0" in message
    assert "secret-token" not in message
    assert "https://" not in message


def test_accepts_unfiltered_provider_without_a_cache(tmp_path):
    config_path = tmp_path / "config.yaml"
    config_path.write_text(CONFIG, encoding="utf-8")

    reports = validate_nonempty_provider_filters(
        config_path.read_text(encoding="utf-8"), config_path, tmp_path, ["unfiltered"]
    )

    assert reports == []


def test_rejects_filtered_provider_when_cache_is_unavailable(tmp_path):
    config_path = tmp_path / "config.yaml"
    config_path.write_text(CONFIG, encoding="utf-8")

    with pytest.raises(ProviderFilterError, match="backup"):
        validate_nonempty_provider_filters(
            config_path.read_text(encoding="utf-8"), config_path, tmp_path, ["backup"]
        )

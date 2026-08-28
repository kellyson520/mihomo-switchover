import pytest

from scripts.mihomo_config_patch import patch_quality_targets


SOURCE = """# keep this header and its line endings
mixed-port: 7890
http-port: 7891
proxy-groups:
  - name: MAIN
    type: url-test
    use: [main-provider]
    url: https://api.openai.com/v1/models
    interval: 300
  - name: BACKUP-USA
    type: select
    proxies: [us-1, us-2]
  - name: EU-RESERVE
    type: select
    proxies:
      - eu-1
      - eu-2
  - name: CHANNEL
    type: select
    proxies: [MAIN, BACKUP-USA]

proxy-providers:
  main-provider:
    type: http
    url: https://provider.example/main.yaml
ports:
  keep: 1234
networks:
  keep: true
rules:
  - DOMAIN-SUFFIX,example.com,DIRECT
listeners:
  - name: user-listener
    type: mixed
    listen: 127.0.0.1
    port: 18000
    proxy: DIRECT
mode: rule
"""


TARGETS = [
    {
        "id": "primary",
        "source_group": "MAIN",
        "provider": "main-provider",
        "scope": "locked",
        "lock_key": "main",
        "listener": "http://127.0.0.1:17990",
    },
    {
        "id": "reserve-us",
        "source_group": "BACKUP-USA",
        "scope": "all",
        "node_filter": "美国",
        "listener": "http://127.0.0.1:17991",
    },
    {
        "id": "eu",
        "source_group": "EU-RESERVE",
        "scope": "all",
        "listener": "http://127.0.0.1:17992",
    },
]


def test_injects_isolated_provider_and_static_quality_targets_without_touching_user_yaml():
    got = patch_quality_targets(SOURCE, TARGETS)

    assert "name: GUARDIAN-QUALITY-primary" in got
    assert "name: GUARDIAN-QUALITY-reserve-us" in got
    assert "name: GUARDIAN-QUALITY-eu" in got
    assert "guardian-quality-primary" in got
    assert "guardian-quality-reserve-us" in got
    assert "guardian-quality-eu" in got
    assert "    use: [main-provider]" in got
    assert "      - us-1\n      - us-2" in got
    assert "      - eu-1\n      - eu-2" in got
    assert "    type: mixed" in got
    assert "    listen: 127.0.0.1" in got
    assert "    port: 17990" in got

    for marker in (
        "name: MAIN",
        "name: BACKUP-USA",
        "name: EU-RESERVE",
        "name: CHANNEL",
        "proxy-providers:",
        "ports:\n  keep: 1234",
        "networks:\n  keep: true",
        "rules:\n  - DOMAIN-SUFFIX,example.com,DIRECT",
        "mode: rule",
        "name: user-listener",
    ):
        assert marker in got


def test_quality_patch_is_idempotent_and_replaces_only_owned_blocks():
    once = patch_quality_targets(SOURCE, TARGETS)
    twice = patch_quality_targets(once, TARGETS)

    assert twice == once

    changed = once.replace("    port: 17991", "    port: 17993", 1)
    changed = patch_quality_targets(changed, TARGETS)
    assert "name: guardian-quality-reserve-us" in changed
    assert "    port: 17991" in changed
    assert "    port: 17993" not in changed


@pytest.mark.parametrize(
    "mutate, message",
    [
        (
            lambda text: text.replace(
                "  - name: CHANNEL", "  - name: GUARDIAN-QUALITY-primary\n    type: select\n    proxies: [DIRECT]\n  - name: CHANNEL"
            ),
            "collision",
        ),
        (
            lambda text: text.replace(
                "  - name: user-listener", "  - name: guardian-quality-primary"
            ),
            "collision",
        ),
        (
            lambda text: text,
            "duplicate",
        ),
        (
            lambda text: text.replace('"source_group": "EU-RESERVE"', '"source_group": "MISSING"'),
            "source group",
        ),
    ],
)
def test_quality_patch_fails_closed_for_collisions_duplicate_ports_and_missing_groups(
    mutate, message
):
    if message == "source group":
        targets = [dict(target) for target in TARGETS]
        targets[2]["source_group"] = "MISSING"
        with pytest.raises(ValueError, match=message):
            patch_quality_targets(SOURCE, targets)
        return
    if message == "duplicate":
        targets = [dict(target) for target in TARGETS]
        targets[1]["listener"] = "http://127.0.0.1:17990"
        with pytest.raises(ValueError, match=message):
            patch_quality_targets(SOURCE, targets)
    else:
        with pytest.raises(ValueError, match=message):
            patch_quality_targets(mutate(SOURCE), TARGETS)


def test_quality_patch_rejects_non_loopback_listener_and_empty_static_group():
    bad_listener = [dict(target) for target in TARGETS]
    bad_listener[0]["listener"] = "http://192.0.2.1:17990"
    with pytest.raises(ValueError, match="loopback"):
        patch_quality_targets(SOURCE, bad_listener)

    empty = SOURCE.replace(
        "  - name: EU-RESERVE\n    type: select\n    proxies:\n      - eu-1\n      - eu-2\n",
        "  - name: EU-RESERVE\n    type: select\n    proxies: []\n",
    )
    with pytest.raises(ValueError, match="static|proxy|empty"):
        patch_quality_targets(empty, TARGETS)


def test_quality_patch_preserves_crlf_and_allows_existing_owned_blocks():
    source = patch_quality_targets(SOURCE.replace("\n", "\r\n"), TARGETS)
    assert "\r\n" in source
    assert "\n" not in source.replace("\r\n", "")
    assert patch_quality_targets(source, TARGETS) == source

from scripts.mihomo_config_patch import (
    patch_file,
    patch_provider_groups,
    patch_quality_targets,
)


def test_patch_changes_only_discovered_provider_groups_to_select():
    source = """mixed-port: 7890
proxy-groups:
  - name: MAIN
    type: url-test
    use:
      - main-channel
    url: https://api.openai.com/v1/models
    expected-status: \"200-499\"
    interval: 300
    tolerance: 100
  - name: BACKUP-USA
    type: url-test
    use:
      - backup-channel
    url: https://api.openai.com/v1/models
    expected-status: \"200-499\"
    interval: 300
  - name: CHANNEL
    type: select
    proxies:
      - MAIN
      - BACKUP-USA
"""
    got = patch_provider_groups(source, "MAIN", "BACKUP-USA")
    assert "name: MAIN\n    type: select" in got
    assert "name: BACKUP-USA\n    type: select" in got
    assert "- main-channel" in got
    assert "url:" not in got
    assert "tolerance:" not in got
    assert "name: CHANNEL\n    type: select" in got


def test_patch_is_idempotent_and_refuses_missing_group():
    source = "proxy-groups:\n  - name: MAIN\n    type: select\n    use:\n      - main-channel\n"
    assert patch_provider_groups(source, "MAIN", "") == source
    try:
        patch_provider_groups(source, "MAIN", "BACKUP-USA")
    except ValueError as exc:
        assert "BACKUP-USA" in str(exc)
    else:
        raise AssertionError("missing backup group was accepted")


def test_quality_target_uses_explicit_provider_instead_of_source_group_provider():
    source = """proxy-groups:
  - name: USER-SOURCE
    type: url-test
    use: [old-provider]
    url: https://example.test/health
  - name: CHANNEL
    type: select
    proxies: [USER-SOURCE, DIRECT]
proxy-providers:
  new-provider:
    type: http
listeners:
  - name: user
    type: mixed
    listen: 127.0.0.1
    port: 18000
    proxy: DIRECT
"""
    got = patch_quality_targets(
        source,
        [
            {
                "id": "custom",
                "source_group": "USER-SOURCE",
                "provider": "new-provider",
                "scope": "all",
                "listener": "http://127.0.0.1:17990",
            }
        ],
    )
    assert "name: GUARDIAN-QUALITY-custom" in got
    quality = got.split("name: GUARDIAN-QUALITY-custom", 1)[1]
    assert "use:\n      - new-provider" in quality
    assert "old-provider" not in quality


def test_patch_file_preserves_crlf_and_file_mode_when_injecting_quality(tmp_path):
    path = tmp_path / "config.yaml"
    source = """proxy-groups:
  - name: MAIN
    type: select
    use: [main-provider]
  - name: CHANNEL
    type: select
    proxies: [MAIN, DIRECT]
proxy-providers:
  main-provider:
    type: http
""".replace("\n", "\r\n")
    path.write_bytes(source.encode())
    path.chmod(0o640)

    patch_file(
        str(path),
        "MAIN",
        "",
        [
            {
                "id": "primary",
                "source_group": "MAIN",
                "provider": "main-provider",
                "listener": "http://127.0.0.1:17990",
            }
        ],
    )

    result = path.read_bytes()
    assert b"\r\n" in result
    assert b"\n" not in result.replace(b"\r\n", b"")
    assert path.stat().st_mode & 0o777 == 0o640

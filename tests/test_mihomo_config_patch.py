from scripts.mihomo_config_patch import patch_provider_groups


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

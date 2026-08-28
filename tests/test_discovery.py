import json
import subprocess
import sys
from dataclasses import asdict
from pathlib import Path

import pytest


try:
    import scripts.discover as discover_module
    from scripts.discover import (
        Discovery,
        discover_from_texts,
        load_discovery,
        main,
        render_guardian_config,
        discover_quality_ports,
        prepare_quality_targets,
        quality_targets_from_text,
        read_proc_socket_ports,
    )
except ModuleNotFoundError as exc:
    if exc.name not in {"scripts", "scripts.discover"}:
        raise
    Discovery = None
    discover_from_texts = None
    load_discovery = None
    main = None
    render_guardian_config = None
    discover_quality_ports = None
    prepare_quality_targets = None
    quality_targets_from_text = None
    read_proc_socket_ports = None
    discover_module = None
    _DISCOVERY_MISSING = True
else:
    _DISCOVERY_MISSING = False


COMPOSE = """
services:
  mihomo-cliproxy:
    image: metacubex/mihomo:Alpha
    container_name: mihomo-cliproxy
    restart: unless-stopped
    volumes:
      - ./config/config.yaml:/root/.config/mihomo/config.yaml:ro
      - ./providers:/root/.config/mihomo/providers
      - ./guardian:/guardian
      - ./guardian/controller_secret:/root/.config/mihomo/.controller_secret:ro
    ports:
      - "127.0.0.1:17890:7890"
      - "127.0.0.1:19090:9090"
  metrics:
    image: prom/prometheus
"""

CONFIG = """
mixed-port: 7890
http-port: 7891
socks-port: 7892
external-controller: 0.0.0.0:9090
secret: super-secret-token

proxy-groups:
  - name: MAIN
    type: url-test
    use: [provider-main]
    url: https://api.openai.com/v1/models
    expected-status: "200-499"
    interval: 300
  - name: BACKUP-USA
    type: url-test
    use:
      - provider-backup
    url: https://api.openai.com/v1/models
    expected-status: "200-499"
    interval: 300
  - name: CHANNEL
    type: select
    proxies: [MAIN, BACKUP-USA]
  - name: PROXY
    type: select
    proxies: [CHANNEL, DIRECT]

proxy-providers:
  provider-main:
    type: http
    url: https://provider.example/main.yaml
  provider-backup:
    type: http
    url: https://provider.example/backup.yaml
"""

INSPECT = [
    {
        "Name": "/mihomo-cliproxy",
        "ContainerName": "mihomo-cliproxy",
        "Config": {
            "Image": "metacubex/mihomo:Alpha",
            "Labels": {
                "com.docker.compose.service": "mihomo-cliproxy",
                "com.docker.compose.project.working_dir": "/srv/mihomo",
                "com.docker.compose.project.config_files": "/srv/mihomo/compose.yml",
            }
        },
        "Mounts": [
            {
                "Type": "bind",
                "Source": "/srv/mihomo/config/config.yaml",
                "Destination": "/root/.config/mihomo/config.yaml",
                "Mode": "ro",
            },
            {
                "Type": "bind",
                "Source": "/srv/mihomo/providers",
                "Destination": "/root/.config/mihomo/providers",
            },
            {
                "Type": "bind",
                "Source": "/srv/mihomo/guardian",
                "Destination": "/guardian",
            },
            {
                "Type": "bind",
                "Source": "/srv/mihomo/guardian/controller_secret",
                "Destination": "/root/.config/mihomo/.controller_secret",
                "Mode": "ro",
            },
        ],
    }
]


def _config(*, mixed="7890", http="7891", socks="7892", controller="0.0.0.0:9090"):
    return f"""
{'' if mixed is None else f'mixed-port: {mixed}'}
{'' if http is None else f'http-port: {http}'}
{'' if socks is None else f'socks-port: {socks}'}
external-controller: {controller}
secret: super-secret-token
proxy-groups:
  - name: MAIN
    type: url-test
    use: [provider-main]
    proxies: [main-node]
  - name: BACKUP-USA
    type: url-test
    use: [provider-backup]
    proxies: [backup-node]
  - name: CHANNEL
    type: select
    proxies: [MAIN, BACKUP-USA]
proxy-providers:
  provider-main:
    type: http
    url: https://provider.example/main.yaml
  provider-backup:
    type: http
    url: https://provider.example/backup.yaml
"""


def _write_inputs(tmp_path, inspect=INSPECT):
    compose_path = tmp_path / "docker-compose.yml"
    config_path = tmp_path / "config" / "config.yaml"
    inspect_path = tmp_path / "inspect.json"
    config_path.parent.mkdir()
    compose_path.write_text(COMPOSE)
    config_path.write_text(CONFIG)
    if inspect is not None:
        aligned_inspect = json.loads(json.dumps(inspect))
        for item in aligned_inspect:
            for mount in item.get("Mounts", []):
                if isinstance(mount.get("Source"), str):
                    mount["Source"] = mount["Source"].replace(
                        "/srv/mihomo", str(tmp_path)
                    )
        inspect_path.write_text(json.dumps(aligned_inspect))
    return compose_path, config_path, inspect_path


def _inspect_variant(*, name=None, service=None, image=None, mounts=None):
    value = json.loads(json.dumps(INSPECT))
    item = value[0]
    if name is not None:
        item["Name"] = name
    if service is not None:
        item["Config"]["Labels"]["com.docker.compose.service"] = service
    if image is not None:
        item["Config"]["Image"] = image
    if mounts is not None:
        item["Mounts"] = mounts
    return value


def _without_secret_mounts():
    return [
        {
            key: value
            for key, value in mount.items()
            if key not in {"Mode"}
        }
        for mount in INSPECT[0]["Mounts"]
        if mount["Destination"] != "/root/.config/mihomo/.controller_secret"
    ]


@pytest.mark.skipif(not _DISCOVERY_MISSING, reason="the discovery module is implemented")
def test_discovery_module_is_available():
    assert not _DISCOVERY_MISSING, "scripts.discover is not implemented"


@pytest.mark.skipif(_DISCOVERY_MISSING, reason="waiting for discovery implementation")
def test_discovers_actual_container_config_ports_groups_providers_and_mounts():
    discovery = discover_from_texts(COMPOSE, CONFIG, inspect=INSPECT, cwd="/srv/mihomo")

    assert isinstance(discovery, Discovery)
    assert discovery.service_name == "mihomo-cliproxy"
    assert discovery.container_name == "mihomo-cliproxy"
    assert discovery.compose_path == "/srv/mihomo/compose.yml"
    assert discovery.config_path == "/srv/mihomo/config/config.yaml"
    assert discovery.providers_dir == "/srv/mihomo/providers"
    assert discovery.host_secret_file == "/srv/mihomo/guardian/controller_secret"
    assert discovery.secret_file == "/root/.config/mihomo/.controller_secret"
    assert "/srv/mihomo/guardian" in discovery.persistence_root_candidates

    assert discovery.mixed_port == 7890
    assert discovery.http_port == 7891
    assert discovery.socks_port == 7892
    assert discovery.api == "http://127.0.0.1:9090"
    assert discovery.proxy == "http://127.0.0.1:7890"
    assert discovery.groups == {
        "channel": "CHANNEL",
        "main": "MAIN",
        "backup": "BACKUP-USA",
    }
    assert discovery.providers["MAIN"] == ("provider-main",)
    assert discovery.providers["BACKUP-USA"] == ("provider-backup",)


@pytest.mark.skipif(_DISCOVERY_MISSING, reason="waiting for discovery implementation")
def test_runtime_ports_are_loopback_and_never_host_published_ports():
    discovery = discover_from_texts(
        COMPOSE,
        _config(mixed="7890", http="7891", socks="7892"),
        inspect=INSPECT,
        cwd="/srv/mihomo",
    )

    assert discovery.proxy == "http://127.0.0.1:7890"
    assert ":17890" not in discovery.proxy
    assert discovery.api == "http://127.0.0.1:9090"
    assert ":19090" not in discovery.api


@pytest.mark.skipif(_DISCOVERY_MISSING, reason="waiting for discovery implementation")
def test_empty_external_controller_host_still_uses_loopback_api():
    discovery = discover_from_texts(
        COMPOSE,
        _config(controller=":9091"),
        inspect=INSPECT,
        cwd="/srv/mihomo",
    )

    assert discovery.api == "http://127.0.0.1:9091"


@pytest.mark.skipif(_DISCOVERY_MISSING, reason="waiting for discovery implementation")
@pytest.mark.parametrize(
    ("mixed", "http", "socks", "expected"),
    [
        (None, "7891", "7892", "http://127.0.0.1:7891"),
        (None, None, "7892", "socks5://127.0.0.1:7892"),
    ],
)
def test_proxy_falls_back_from_mixed_to_http_then_socks(mixed, http, socks, expected):
    discovery = discover_from_texts(
        COMPOSE,
        _config(mixed=mixed, http=http, socks=socks),
        inspect=INSPECT,
        cwd="/srv/mihomo",
    )

    assert discovery.proxy == expected


@pytest.mark.skipif(_DISCOVERY_MISSING, reason="waiting for discovery implementation")
def test_rejects_missing_or_ambiguous_compose_candidates():
    with pytest.raises(ValueError, match="mihomo-cliproxy|candidate|service"):
        discover_from_texts("services:\n  metrics:\n    image: prom/prometheus\n", CONFIG)

    ambiguous = """
services:
  mihomo-cliproxy:
    image: metacubex/mihomo:Alpha
  second-mihomo:
    image: metacubex/mihomo:Alpha
"""
    with pytest.raises(ValueError, match="ambiguous|multiple|unique"):
        discover_from_texts(ambiguous, CONFIG)


@pytest.mark.skipif(_DISCOVERY_MISSING, reason="waiting for discovery implementation")
def test_rejects_ambiguous_inspect_candidates():
    inspect = [
        {"Name": "/mihomo-cliproxy", "Config": {"Labels": {}}},
        {
            "Name": "/mihomo-cliproxy-2",
            "Config": {"Labels": {"com.docker.compose.service": "mihomo-cliproxy"}},
        },
    ]
    with pytest.raises(ValueError, match="ambiguous|multiple|unique"):
        discover_from_texts(COMPOSE, CONFIG, inspect=inspect)


@pytest.mark.skipif(_DISCOVERY_MISSING, reason="waiting for discovery implementation")
@pytest.mark.parametrize(
    "inspect",
    [
        _inspect_variant(name="/wrong-container"),
        _inspect_variant(service="wrong-service"),
        [{"Config": {"Image": "metacubex/mihomo:Alpha"}}],
    ],
)
def test_rejects_inspect_identity_that_is_missing_or_conflicts(inspect):
    with pytest.raises(ValueError, match="identity|match|name|service|image|inspect"):
        discover_from_texts(COMPOSE, CONFIG, inspect=inspect, cwd="/srv/mihomo")


@pytest.mark.skipif(_DISCOVERY_MISSING, reason="waiting for discovery implementation")
def test_rejects_conflicting_mount_sources_and_ignores_non_target_secret_mounts():
    conflicting_inspect = _inspect_variant(
        mounts=[
            {
                "Type": "bind",
                "Source": "/different/config.yaml",
                "Destination": "/root/.config/mihomo/config.yaml",
            }
        ]
    )
    with pytest.raises(ValueError, match="mount|source|conflict|ambiguous"):
        discover_from_texts(
            COMPOSE,
            CONFIG,
            inspect=conflicting_inspect,
            cwd="/srv/mihomo",
        )

    non_target_secret = _inspect_variant(
        mounts=[
            {
                "Type": "bind",
                "Source": "/srv/mihomo/config/config.yaml",
                "Destination": "/root/.config/mihomo/config.yaml",
            },
            {
                "Type": "bind",
                "Source": "/srv/mihomo/providers",
                "Destination": "/root/.config/mihomo/providers",
            },
            {
                "Type": "bind",
                "Source": "/srv/mihomo/secret-token",
                "Destination": "/tmp/secret-token",
            },
        ]
    )
    discovery = discover_from_texts(
        COMPOSE.replace(
            "      - ./guardian/controller_secret:/root/.config/mihomo/.controller_secret:ro\n",
            "",
        ),
        CONFIG,
        inspect=non_target_secret,
        cwd="/srv/mihomo",
    )
    assert discovery.secret_file is None
    assert discovery.host_secret_file is None


@pytest.mark.skipif(_DISCOVERY_MISSING, reason="waiting for discovery implementation")
def test_requires_declared_proxy_provider_and_non_empty_provider_use():
    missing_provider = CONFIG.replace("  provider-backup:\n", "  other-provider:\n")
    with pytest.raises(ValueError, match="provider|proxy-providers|declared"):
        discover_from_texts(COMPOSE, missing_provider)

    empty_use = CONFIG.replace("use: [provider-main]", "use: []")
    with pytest.raises(ValueError, match="use|provider|empty"):
        discover_from_texts(COMPOSE, empty_use)


@pytest.mark.skipif(_DISCOVERY_MISSING, reason="waiting for discovery implementation")
@pytest.mark.parametrize(
    "proxies",
    [
        "[MAIN, BACKUP-USA, DIRECT]",
        "[MAIN, MAIN]",
        "[MAIN, DIRECT]",
    ],
)
def test_channel_must_select_exactly_the_two_discovered_groups(proxies):
    config = CONFIG.replace("proxies: [MAIN, BACKUP-USA]", f"proxies: {proxies}")
    with pytest.raises(ValueError, match="channel|proxy|group|relationship|exactly"):
        discover_from_texts(COMPOSE, config)


@pytest.mark.skipif(_DISCOVERY_MISSING, reason="waiting for discovery implementation")
@pytest.mark.parametrize(
    "config",
    [
        _config(mixed="${" + "MIXED_PORT:-7890}"),
        _config(controller="9090"),
        _config(mixed="7890:7891"),
    ],
)
def test_rejects_unknown_or_ambiguous_ports(config):
    with pytest.raises(ValueError, match="port|controller|ambiguous|integer"):
        discover_from_texts(COMPOSE, config, inspect=INSPECT, cwd="/srv/mihomo")


@pytest.mark.skipif(_DISCOVERY_MISSING, reason="waiting for discovery implementation")
def test_rejects_duplicate_or_ambiguous_channel_relationships():
    duplicate_name = CONFIG.replace(
        "  - name: CHANNEL\n",
        "  - name: CHANNEL\n    # duplicate below\n  - name: CHANNEL\n",
        1,
    )
    with pytest.raises(ValueError, match="duplicate|ambiguous|group"):
        discover_from_texts(COMPOSE, duplicate_name)

    two_channels = CONFIG.replace(
        "  - name: PROXY\n",
        "  - name: CHANNEL-2\n    type: select\n    proxies: [MAIN, BACKUP-USA]\n  - name: PROXY\n",
    )
    with pytest.raises(ValueError, match="ambiguous|channel|relationship|group"):
        discover_from_texts(COMPOSE, two_channels)

    bad_relation = CONFIG.replace("proxies: [MAIN, BACKUP-USA]", "proxies: [MAIN]")
    with pytest.raises(ValueError, match="channel|main|backup|relationship|group"):
        discover_from_texts(COMPOSE, bad_relation)


@pytest.mark.skipif(_DISCOVERY_MISSING, reason="waiting for discovery implementation")
def test_secret_is_not_present_in_repr_or_serialized_public_data():
    discovery = discover_from_texts(COMPOSE, CONFIG, inspect=INSPECT, cwd="/srv/mihomo")

    public_text = repr(discovery) + str(discovery)
    assert "super-secret-token" not in public_text
    assert discovery.secret == "super-secret-token"
    assert "secret" not in discovery.public_dict()
    assert "super-secret-token" not in repr(getattr(discovery, "__dict__", {}))
    assert "super-secret-token" not in repr(asdict(discovery))
    assert discovery.secret_configured is True


@pytest.mark.skipif(_DISCOVERY_MISSING, reason="waiting for discovery implementation")
def test_render_replaces_only_infrastructure_and_preserves_behavior_sections():
    discovery = discover_from_texts(COMPOSE, CONFIG, inspect=INSPECT, cwd="/srv/mihomo")
    template = """mihomo:
  api: http://127.0.0.1:1
  proxy: http://127.0.0.1:2
  secret_file: /old/secret
groups:
  channel: OLD_CHANNEL
  main: OLD_MAIN
  backup: OLD_BACKUP

providers:
  main: old-provider
  backup: old-provider

decision:
  mode: auto
  interval: 15s
probes:
  - id: openai
    url: https://api.openai.com/v1/models
logging:
  retain: 7
"""

    rendered = render_guardian_config(template, discovery)

    assert "api: http://127.0.0.1:9090" in rendered
    assert "proxy: http://127.0.0.1:7890" in rendered
    assert "secret_file: /root/.config/mihomo/.controller_secret" in rendered
    assert "channel: CHANNEL" in rendered
    assert "main: MAIN" in rendered
    assert "backup: BACKUP-USA" in rendered
    assert "main: provider-main" in rendered
    assert "backup: provider-backup" in rendered
    assert "url: https://api.openai.com/v1/models" in rendered
    assert "mode: auto" in rendered
    assert "interval: 15s" in rendered
    assert "retain: 7" in rendered
    assert "super-secret-token" not in rendered


@pytest.mark.skipif(_DISCOVERY_MISSING, reason="waiting for discovery implementation")
def test_render_removes_stale_secret_file_when_discovery_has_no_secret_mount():
    discovery = discover_from_texts(
        COMPOSE.replace(
            "      - ./guardian/controller_secret:/root/.config/mihomo/.controller_secret:ro\n",
            "",
        ),
        CONFIG,
        inspect=_inspect_variant(mounts=_without_secret_mounts()),
        cwd="/srv/mihomo",
    )
    template = """mihomo:
  api: http://127.0.0.1:1
  proxy: http://127.0.0.1:2
  secret_file: /stale/secret
groups:
  channel: OLD_CHANNEL
  main: OLD_MAIN
  backup: OLD_BACKUP
"""

    rendered = render_guardian_config(template, discovery)

    assert "secret_file:" not in rendered
    assert "/stale/secret" not in rendered


@pytest.mark.skipif(_DISCOVERY_MISSING, reason="waiting for discovery implementation")
def test_render_writes_prepared_quality_listeners_without_overwriting_other_quality_fields():
    discovery = discover_from_texts(COMPOSE, CONFIG, inspect=INSPECT, cwd="/srv/mihomo")
    template = """mihomo:
  api: http://127.0.0.1:1
  proxy: http://127.0.0.1:2
groups:
  channel: CHANNEL
  main: MAIN
  backup: BACKUP-USA
providers:
  main: provider-main
  backup: provider-backup
quality:
  enabled: true
  targets:
    - id: primary
      source_group: MAIN
      listener: http://127.0.0.1:17990
      node_filter: "keep-this-filter"
    - id: reserve
      source_group: BACKUP-USA
      node_filter: "keep-this-reserve-filter"
decision:
  mode: observe
"""

    rendered = render_guardian_config(
        template,
        discovery,
        quality_targets=[
            {
                "id": "primary",
                "source_group": "MAIN",
                "listener": "http://127.0.0.1:18190",
            },
            {
                "id": "reserve",
                "source_group": "BACKUP-USA",
                "listener": "http://127.0.0.1:18191",
            },
        ],
    )

    assert "listener: http://127.0.0.1:18190" in rendered
    assert "listener: http://127.0.0.1:18191" in rendered
    assert 'node_filter: "keep-this-filter"' in rendered
    assert 'node_filter: "keep-this-reserve-filter"' in rendered


@pytest.mark.skipif(_DISCOVERY_MISSING, reason="waiting for discovery implementation")
def test_load_discovery_reads_paths_and_json_inspect(tmp_path):
    compose_path, config_path, inspect_path = _write_inputs(tmp_path)

    discovery = load_discovery(compose_path, config_path, inspect_path)

    assert discovery.compose_path == str(compose_path)
    assert discovery.config_path == str(config_path)
    assert discovery.proxy == "http://127.0.0.1:7890"


@pytest.mark.skipif(_DISCOVERY_MISSING, reason="waiting for discovery implementation")
def test_cli_auto_finds_unique_paths_and_inspects_container(tmp_path, monkeypatch, capsys):
    _, config_path, inspect_path = _write_inputs(tmp_path)
    aligned_inspect = inspect_path.read_text()

    calls = []

    def fake_run(args, **kwargs):
        calls.append(args)
        if args[:3] == ["docker", "ps", "-a"]:
            return subprocess.CompletedProcess(args, 0, "mihomo-cliproxy\n", "")
        if args[:2] == ["docker", "inspect"]:
            return subprocess.CompletedProcess(args, 0, aligned_inspect, "")
        raise AssertionError(f"unexpected command: {args}")

    monkeypatch.setattr(discover_module.subprocess, "run", fake_run)
    result = main(["--auto", "--cwd", str(tmp_path), "--format", "json"])

    assert result == 0
    payload = json.loads(capsys.readouterr().out)
    assert payload["config_path"] == str(config_path.resolve())
    assert any(args[:3] == ["docker", "ps", "-a"] for args in calls)
    assert any(args[:2] == ["docker", "inspect"] for args in calls)


@pytest.mark.skipif(_DISCOVERY_MISSING, reason="waiting for discovery implementation")
def test_cli_auto_accepts_explicit_container_without_docker_ps(tmp_path, monkeypatch, capsys):
    _, _, inspect_path = _write_inputs(tmp_path)
    aligned_inspect = inspect_path.read_text()
    calls = []

    def fake_run(args, **kwargs):
        calls.append(args)
        if args[:2] == ["docker", "inspect"]:
            return subprocess.CompletedProcess(args, 0, aligned_inspect, "")
        raise AssertionError(f"unexpected command: {args}")

    monkeypatch.setattr(discover_module.subprocess, "run", fake_run)
    result = main(
        [
            "--auto",
            "--cwd",
            str(tmp_path),
            "--container",
            "mihomo-cliproxy",
            "--format",
            "env",
        ]
    )

    assert result == 0
    assert "MIHOMO_API=http://127.0.0.1:9090" in capsys.readouterr().out
    assert calls == [["docker", "inspect", "mihomo-cliproxy"]]


@pytest.mark.skipif(_DISCOVERY_MISSING, reason="waiting for discovery implementation")
def test_cli_json_and_env_are_safe_for_install_scripts(tmp_path):
    compose_path, config_path, inspect_path = _write_inputs(tmp_path)
    script = Path(__file__).parents[1] / "scripts" / "discover.py"

    json_result = subprocess.run(
        [
            sys.executable,
            str(script),
            "--compose",
            str(compose_path),
            "--config",
            str(config_path),
            "--inspect-json",
            str(inspect_path),
            "--format",
            "json",
        ],
        check=True,
        capture_output=True,
        text=True,
    )
    payload = json.loads(json_result.stdout)
    assert payload["api"] == "http://127.0.0.1:9090"
    assert payload["proxy"] == "http://127.0.0.1:7890"
    assert payload["has_secret"] is True
    assert payload["secret_file"] == "/root/.config/mihomo/.controller_secret"
    assert "secret" not in payload
    assert "super-secret-token" not in json_result.stdout

    env_result = subprocess.run(
        [
            sys.executable,
            str(script),
            "--compose",
            str(compose_path),
            "--config",
            str(config_path),
            "--inspect-json",
            str(inspect_path),
            "--format",
            "env",
        ],
        check=True,
        capture_output=True,
        text=True,
    )
    assert "MIHOMO_API=http://127.0.0.1:9090" in env_result.stdout
    assert "MIHOMO_PROXY=http://127.0.0.1:7890" in env_result.stdout
    assert "MIHOMO_SECRET_FILE=/root/.config/mihomo/.controller_secret" in env_result.stdout
    assert "MIHOMO_HAS_SECRET=1" in env_result.stdout
    assert "super-secret-token" not in env_result.stdout


def test_quality_port_discovery_prefers_existing_ports_and_skips_config_and_socket_ports():
    config = """mixed-port: 7890
http-port: 7891
external-controller: 127.0.0.1:9090
proxy-groups:
  - name: MAIN
    type: select
    proxies: [DIRECT]
listeners:
  - name: guardian-quality-old
    # mihomo-guardian: generated quality target old
    type: mixed
    listen: 127.0.0.1
    port: 17990
    proxy: GUARDIAN-QUALITY-old
  - name: user
    type: mixed
    listen: 127.0.0.1
    port: 17991
    proxy: DIRECT
"""
    targets = [
        {"id": "old", "source_group": "MAIN", "listener": ""},
        {"id": "new", "source_group": "MAIN", "listener": ""},
    ]
    tcp = "  sl\n  0100007F:4648 00000000:0000 0A 00000000:0000 00:00000000 00000000   100        0 1 2\n"
    tcp6 = "  sl\n  00000000000000000000000000000000:4642 00000000000000000000000000000000:0000 0A 0\n"

    discovered = discover_quality_ports(
        config,
        targets,
        proc_tcp=tcp,
        proc_tcp6=tcp6,
    )
    assert discovered == [17990, 17993]

    prepared = prepare_quality_targets(
        config,
        targets,
        proc_tcp=tcp,
        proc_tcp6=tcp6,
    )
    assert [target["listener"] for target in prepared] == [
        "http://127.0.0.1:17990",
        "http://127.0.0.1:17993",
    ]


def test_quality_port_discovery_fails_closed_without_socket_tables_or_on_user_port_conflict():
    target = [{"id": "one", "source_group": "MAIN", "listener": ""}]
    with pytest.raises(ValueError, match="socket"):
        discover_quality_ports("proxy-groups:\n  - name: MAIN\n", target, proc_tcp=None, proc_tcp6=None)

    with pytest.raises(ValueError, match="port"):
        discover_quality_ports(
            "mixed-port: 17990\nproxy-groups:\n  - name: MAIN\n",
            [{**target[0], "listener": "http://127.0.0.1:17990"}],
            proc_tcp="sl\n",
            proc_tcp6="sl\n",
        )


@pytest.mark.skipif(_DISCOVERY_MISSING, reason="waiting for discovery implementation")
def test_quality_targets_from_example_config_is_safe_to_use_for_first_install():
    example = Path(__file__).parents[1] / "configs" / "guardian.example.yaml"

    targets = quality_targets_from_text(example.read_text(encoding="utf-8"))

    assert targets == []


@pytest.mark.skipif(_DISCOVERY_MISSING, reason="waiting for discovery implementation")
def test_quality_flow_style_inline_mapping_fails_closed_instead_of_being_skipped():
    with pytest.raises(ValueError, match="inline|block|flow"):
        quality_targets_from_text(
            "quality: {enabled: true, targets: [{id: primary, source_group: MAIN}]}\n"
        )


@pytest.mark.skipif(_DISCOVERY_MISSING, reason="waiting for discovery implementation")
def test_proc_socket_parser_rejects_unknown_nonempty_rows():
    with pytest.raises(ValueError, match="socket table|unsupported"):
        read_proc_socket_ports("sl\ngarbage\n", "sl\n")


def test_install_contains_read_only_quality_preflight_and_quality_patch_path():
    script = (Path(__file__).parents[1] / "scripts" / "install.sh").read_text()
    assert "prepare_quality_targets" in script
    assert "patch_quality_targets" in script
    assert 'docker exec "$CONTAINER" cat /proc/net/tcp' in script
    assert 'docker exec "$CONTAINER" cat /proc/net/tcp6' in script
    assert "preflight=ok (no files, services, or containers changed)" in script
    assert "patched = patch_quality_targets(patched, targets)" in script
    assert "quality_targets=quality_targets" in script

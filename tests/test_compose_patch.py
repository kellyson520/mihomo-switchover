from pathlib import Path

import pytest

from scripts.compose_patch import patch_compose


def test_patch_is_idempotent_and_preserves_existing_service():
    source = (Path(__file__).parent / "fixtures" / "mihomo-compose.yml").read_text()
    once = patch_compose(source, "/opt/mihomo-cliproxy/guardian")
    twice = patch_compose(once, "/opt/mihomo-cliproxy/guardian")

    assert twice == once
    assert "127.0.0.1:7891:7890" in twice
    assert "1panel-network" in twice
    assert "/opt/mihomo-cliproxy/providers:/root/.config/mihomo/providers" in twice
    assert 'entrypoint: ["/bin/sh", "/guardian/start-guardian.sh"]' in twice
    assert "/opt/mihomo-cliproxy/guardian/data:/guardian/data" in twice
    assert "/opt/mihomo-cliproxy/guardian/controller_secret:/guardian/controller_secret:ro" in twice


def test_patch_migrates_legacy_guardian_file_mount_to_directory_without_touching_mihomo_fields():
    source = """services:
  mihomo-cliproxy:
    image: metacubex/mihomo:Alpha
    container_name: mihomo-cliproxy
    command: [\"-d\", \"/root/.config/mihomo\"]
    network_mode: host
    environment:
      MIHOMO_LOG_LEVEL: info
    volumes:
      - /opt/x/guardian/bin/guardian:/guardian/bin/guardian:ro
      - /opt/x/providers:/root/.config/mihomo/providers:rw
    ports:
      - 127.0.0.1:7891:7890
"""

    patched = patch_compose(source, "/opt/x/guardian")

    assert "- /opt/x/guardian/bin:/guardian/bin:ro" in patched
    assert "/guardian/bin/guardian:" not in patched
    assert "127.0.0.1:7891:7890" in patched
    assert "network_mode: host" in patched
    assert "MIHOMO_LOG_LEVEL: info" in patched
    assert "command: []" in patched
    assert 'entrypoint: ["/bin/sh", "/guardian/start-guardian.sh"]' in patched
    assert patch_compose(patched, "/opt/x/guardian") == patched


def test_patch_rejects_conflicting_guardian_directory_mount():
    source = """services:
  mihomo-cliproxy:
    image: x
    volumes:
      - /opt/other/bin:/guardian/bin:ro
"""

    with pytest.raises(ValueError, match="guardian/bin"):
        patch_compose(source, "/opt/x/guardian")


def test_patch_rejects_writable_guardian_directory_mount():
    source = """services:
  mihomo-cliproxy:
    image: x
    volumes:
      - /opt/x/guardian/bin:/guardian/bin:rw
"""

    with pytest.raises(ValueError, match="read-only"):
        patch_compose(source, "/opt/x/guardian")


def test_patch_rejects_long_form_volume_syntax_instead_of_duplicating_mounts():
    source = """services:
  mihomo-cliproxy:
    image: x
    volumes:
      - type: bind
        source: /opt/x/guardian/bin
        target: /guardian/bin
        read_only: true
"""

    with pytest.raises(ValueError, match="long-form volume"):
        patch_compose(source, "/opt/x/guardian")


def test_patch_rejects_compound_guardian_mount_mode():
    source = """services:
  mihomo-cliproxy:
    image: x
    volumes:
      - /opt/x/guardian/bin:/guardian/bin:ro,z
"""

    with pytest.raises(ValueError, match="volume mode"):
        patch_compose(source, "/opt/x/guardian")


def test_patch_rejects_compound_legacy_guardian_mount_mode():
    source = """services:
  mihomo-cliproxy:
    image: x
    volumes:
      - /opt/x/guardian/bin/guardian:/guardian/bin/guardian:ro,z
"""

    with pytest.raises(ValueError, match="volume mode"):
        patch_compose(source, "/opt/x/guardian")


def test_patch_rejects_missing_target_service():
    with pytest.raises(ValueError, match="mihomo-cliproxy"):
        patch_compose("services:\n  other:\n    image: x\n", "/opt/x")


def test_patch_rejects_ambiguous_target_service():
    source = "services:\n  mihomo-cliproxy:\n    image: x\n  other:\n    container_name: mihomo-cliproxy\n"
    with pytest.raises(ValueError, match="uniquely"):
        patch_compose(source, "/opt/x")


def test_patch_rejects_non_mapping_service_body():
    with pytest.raises(ValueError, match="service"):
        patch_compose("services:\n  mihomo-cliproxy: null\n", "/opt/x")

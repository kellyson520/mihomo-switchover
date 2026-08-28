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

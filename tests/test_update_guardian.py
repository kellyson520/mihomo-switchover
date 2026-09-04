from pathlib import Path


SCRIPT_PATH = Path(__file__).parents[1] / "scripts" / "update-guardian.sh"


def _script() -> str:
    assert SCRIPT_PATH.exists(), "update-guardian.sh must be created before these contracts can pass"
    return SCRIPT_PATH.read_text(encoding="utf-8")


def test_update_script_requires_directory_mount_and_preflight_is_read_only():
    script = _script()

    assert 'GUARDIAN_BIN_DESTINATION="/guardian/bin"' in script
    assert 'GUARDIAN_BIN_MODE="directory"' in script
    assert "--preflight" in script
    assert "migration_required=1" in script
    assert "exit 0" in script


def test_update_script_builds_to_temp_verifies_elf_hash_and_renames_atomically():
    script = _script()

    assert "mktemp" in script
    assert "sha256sum" in script
    assert 'file "$BUILD_ARTIFACT"' in script or "readelf" in script
    assert 'mv "$LIVE_TMP" "$LIVE_BINARY"' in script


def test_update_script_only_terms_guardian_children():
    script = _script()

    assert 'kill -TERM "$pid"' in script
    assert "guardian_pid" in script
    assert "quality_pid" in script
    assert 'kill -TERM "$mihomo_pid"' not in script


def test_update_script_preserves_mihomo_pid_and_rolls_back_binary_on_failed_verification():
    script = _script()

    assert "mihomo_pid_before" in script
    assert "mihomo_pid_after" in script
    assert "update_rolled_back" in script
    assert 'mv "$ROLLBACK_TMP" "$LIVE_BINARY"' in script
    assert "docker compose" not in script
    assert "docker stop" not in script
    assert "docker restart" not in script
    assert "docker kill" not in script
    assert "docker compose down" not in script

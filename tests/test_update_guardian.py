from pathlib import Path
import json
import os
import shutil
import subprocess


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


def test_update_preflight_is_read_only_with_a_directory_mount(tmp_path):
    guardian_root = tmp_path / "guardian"
    bin_dir = guardian_root / "bin"
    bin_dir.mkdir(parents=True)
    shutil.copy2("/bin/sh", bin_dir / "guardian")
    (guardian_root / "guardian.yaml").write_text("decision:\n  mode: observe\n", encoding="utf-8")
    (guardian_root / "controller_secret").write_text("redacted\n", encoding="utf-8")
    (guardian_root / "start-guardian.sh").write_text("#!/bin/sh\n", encoding="utf-8")
    (guardian_root / "start-guardian.sh").chmod(0o755)
    (guardian_root / "logs").mkdir()
    tmp_dir = tmp_path / "tmp"
    tmp_dir.mkdir()

    inspect_path = tmp_path / "inspect.json"
    inspect_path.write_text(
        json.dumps(
            [
                {
                    "Id": "container-id",
                    "State": {"Status": "running"},
                    "Mounts": [
                        {
                            "Type": "bind",
                            "Source": str(bin_dir),
                            "Destination": "/guardian/bin",
                            "Mode": "ro",
                            "RW": False,
                        },
                        {
                            "Type": "bind",
                            "Source": str(guardian_root / "guardian.yaml"),
                            "Destination": "/guardian/guardian.yaml",
                            "Mode": "ro",
                            "RW": False,
                        },
                    ],
                }
            ]
        ),
        encoding="utf-8",
    )
    docker_log = tmp_path / "docker.log"
    fake_docker = tmp_path / "docker"
    fake_docker.write_text(
        """#!/bin/sh
set -eu
printf '%s\\n' "$*" >> "$FAKE_DOCKER_LOG"
if [ "$1" = inspect ] && [ "${2:-}" = --format ]; then
    case "$3" in
        *State.Status*) printf 'running\\n' ;;
        *Id*) printf 'container-id\\n' ;;
        *) exit 1 ;;
    esac
    exit 0
fi
if [ "$1" = inspect ]; then
    cat "$FAKE_INSPECT"
    exit 0
fi
if [ "$1" = top ]; then
    printf 'PID PPID COMMAND COMMAND\\n'
    printf '101 1 mihomo /mihomo -d /config\\n'
    printf '102 1 guardian /guardian/bin/guardian run --config /guardian/guardian.yaml\\n'
    exit 0
fi
exit 1
""",
        encoding="utf-8",
    )
    fake_docker.chmod(0o755)

    env = os.environ.copy()
    env["PATH"] = f"{tmp_path}:{env['PATH']}"
    env["FAKE_INSPECT"] = str(inspect_path)
    env["FAKE_DOCKER_LOG"] = str(docker_log)
    env["TMPDIR"] = str(tmp_dir)
    result = subprocess.run(
        [str(SCRIPT_PATH), "--preflight", "--container", "fixture", "--guardian-root", str(guardian_root)],
        cwd=SCRIPT_PATH.parents[1],
        env=env,
        text=True,
        capture_output=True,
        check=False,
    )

    assert result.returncode == 0, result.stderr
    assert "guardian_bin_mount=directory" in result.stdout
    assert "preflight=ok" in result.stdout
    assert not (guardian_root / "logs" / "guardian-update.jsonl").exists()
    assert not (guardian_root / "backups").exists()
    docker_calls = docker_log.read_text(encoding="utf-8")
    assert "kill" not in docker_calls
    assert "run" not in docker_calls
    assert "exec" not in docker_calls


def test_update_replaces_binary_and_keeps_mihomo_pid_with_fake_docker(tmp_path):
    guardian_root = tmp_path / "guardian"
    bin_dir = guardian_root / "bin"
    bin_dir.mkdir(parents=True)
    shutil.copy2("/bin/true", bin_dir / "guardian")
    config = guardian_root / "guardian.yaml"
    config.write_text("decision:\n  mode: observe\n", encoding="utf-8")
    (guardian_root / "controller_secret").write_text("redacted\n", encoding="utf-8")
    (guardian_root / "start-guardian.sh").write_text("#!/bin/sh\n", encoding="utf-8")
    (guardian_root / "start-guardian.sh").chmod(0o755)
    (guardian_root / "logs").mkdir()
    tmp_dir = tmp_path / "tmp"
    tmp_dir.mkdir()
    docker_log = tmp_path / "docker.log"
    docker_state = tmp_path / "docker.state"
    docker_state.write_text("before\n", encoding="utf-8")

    inspect_path = tmp_path / "inspect.json"
    inspect_path.write_text(
        json.dumps(
            [
                {
                    "Id": "container-id",
                    "State": {"Status": "running"},
                    "Mounts": [
                        {
                            "Type": "bind",
                            "Source": str(bin_dir),
                            "Destination": "/guardian/bin",
                            "Mode": "ro",
                            "RW": False,
                        },
                        {
                            "Type": "bind",
                            "Source": str(config),
                            "Destination": "/guardian/guardian.yaml",
                            "Mode": "ro",
                            "RW": False,
                        },
                    ],
                }
            ]
        ),
        encoding="utf-8",
    )
    fake_docker = tmp_path / "docker"
    fake_docker.write_text(
        """#!/bin/sh
set -eu
printf '%s\\n' "$*" >> "$FAKE_DOCKER_LOG"
if [ "$1" = inspect ] && [ "${2:-}" = --format ]; then
    case "$3" in
        *State.Status*) printf 'running\\n' ;;
        *Id*) printf 'container-id\\n' ;;
        *) exit 1 ;;
    esac
    exit 0
fi
if [ "$1" = inspect ]; then
    cat "$FAKE_INSPECT"
    exit 0
fi
if [ "$1" = run ]; then
    for argument in "$@"; do
        case "$argument" in
            *:/build) build_dir=${argument%:/build} ;;
        esac
    done
    cp "$FAKE_ARTIFACT_SOURCE" "$build_dir/guardian"
    exit 0
fi
if [ "$1" = top ]; then
    printf 'PID PPID COMMAND COMMAND\\n'
    printf '101 1 mihomo /mihomo -d /config\\n'
    if grep -q '^after$' "$FAKE_DOCKER_STATE"; then
        printf '103 1 guardian /guardian/bin/guardian run --config /guardian/guardian.yaml\\n'
    else
        printf '102 1 guardian /guardian/bin/guardian run --config /guardian/guardian.yaml\\n'
    fi
    exit 0
fi
if [ "$1" = exec ] && [ "$3" = cat ]; then
    cat "$FAKE_ARTIFACT_SOURCE"
    exit 0
fi
if [ "$1" = exec ] && [ "$3" = kill ]; then
    [ "$5" = 102 ]
    printf 'after\\n' > "$FAKE_DOCKER_STATE"
    exit 0
fi
exit 1
""",
        encoding="utf-8",
    )
    fake_docker.chmod(0o755)

    env = os.environ.copy()
    env["PATH"] = f"{tmp_path}:{env['PATH']}"
    env["FAKE_INSPECT"] = str(inspect_path)
    env["FAKE_DOCKER_LOG"] = str(docker_log)
    env["FAKE_DOCKER_STATE"] = str(docker_state)
    env["FAKE_ARTIFACT_SOURCE"] = "/bin/sh"
    env["TMPDIR"] = str(tmp_dir)
    result = subprocess.run(
        [str(SCRIPT_PATH), "--container", "fixture", "--guardian-root", str(guardian_root)],
        cwd=SCRIPT_PATH.parents[1],
        env=env,
        text=True,
        capture_output=True,
        check=False,
    )

    assert result.returncode == 0, result.stderr
    assert "update=ok" in result.stdout
    assert guardian_root.joinpath("bin/guardian").read_bytes() == Path("/bin/sh").read_bytes()
    manifests = list((guardian_root / "backups").glob("update-*/manifest"))
    assert len(manifests) == 1
    manifest = manifests[0].read_text(encoding="utf-8")
    assert "mihomo_pid=101" in manifest
    events = (guardian_root / "logs" / "guardian-update.jsonl").read_text(encoding="utf-8")
    for event in ("update_started", "binary_backed_up", "guardian_reloaded", "update_verified"):
        assert event in events
    docker_calls = docker_log.read_text(encoding="utf-8")
    assert "exec fixture kill -TERM 102" in docker_calls
    assert "kill -TERM 101" not in docker_calls
    assert "stop" not in docker_calls
    assert "restart" not in docker_calls

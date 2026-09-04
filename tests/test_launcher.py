import os
import subprocess
from pathlib import Path


def test_launcher_restarts_guardian_without_killing_mihomo():
    script = (Path(__file__).parents[1] / "deploy" / "start-guardian.sh").read_text()
    loop = script.split("guardian_loop() {", 1)[1].split("}", 1)[0]
    assert 'kill "$mihomo_pid"' not in loop
    assert 'kill "$mihomo_pid"' in script
    assert "guardian_pid" in script
    assert 'wait "$mihomo_pid"' in script
    assert '"$GUARDIAN_BIN" run' in script


def test_launcher_supervises_quality_daemon_as_an_independent_child_loop():
    script = (Path(__file__).parents[1] / "deploy" / "start-guardian.sh").read_text()
    assert "quality_loop() {" in script
    assert '"$GUARDIAN_BIN" quality-daemon' in script
    assert 'quality_pid' in script
    assert 'current_quality_pid' in script
    quality_loop = script.split("quality_loop() {", 1)[1].split("}\n\n", 1)[0]
    assert 'kill "$mihomo_pid"' not in quality_loop
    assert 'wait "$current_quality_pid"' in quality_loop
    assert '--config "$GUARDIAN_CONFIG"' in quality_loop
    assert '--data "$GUARDIAN_DATA"' in quality_loop
    assert '--logs "$GUARDIAN_LOGS"' in quality_loop
    assert '--secret-file "$GUARDIAN_SECRET"' in quality_loop
    shutdown = script.split("shutdown() {", 1)[1].split("}\n\n", 1)[0]
    assert 'kill "$quality_pid"' in shutdown
    assert 'wait "$quality_pid"' in shutdown


def test_launcher_cleans_quality_loop_when_mihomo_exits():
    script = (Path(__file__).parents[1] / "deploy" / "start-guardian.sh").read_text()
    after_mihomo_wait = script.split('wait "$mihomo_pid"', 1)[1]
    assert 'kill "$quality_pid"' in after_mihomo_wait
    assert 'wait "$quality_pid"' in after_mihomo_wait


def test_install_is_discovery_driven_and_preflight_is_read_only():
    script = (Path(__file__).parents[1] / "scripts" / "install.sh").read_text()
    assert "scripts/discover.py" in script
    assert "--preflight" in script
    assert "docker compose" in script
    assert 'docker compose -f "$COMPOSE_PATH" up -d --force-recreate "$DISCOVERED_SERVICE"' in script
    assert 'kill "$mihomo_pid"' not in script


def test_install_smoke_disables_environment_proxy_without_bypassing_mihomo():
    script = (Path(__file__).parents[1] / "scripts" / "install.sh").read_text()
    smoke = script.split("smoke_code=$(curl", 1)[1].split(" ||", 1)[0]
    assert "--noproxy ''" in smoke
    assert "--proxy \"http://127.0.0.1:$proxy_host_port\"" in smoke
    assert "--noproxy '*'" not in smoke


def test_rollback_searches_nested_backup_manifests():
    script = (Path(__file__).parents[1] / "scripts" / "rollback.sh").read_text()
    assert "-mindepth 2 -maxdepth 2 -type f -name manifest" in script


def test_install_health_check_uses_container_pid_namespace():
    script = (Path(__file__).parents[1] / "scripts" / "install.sh").read_text()
    wait_check = script.split("wait_for_runtime()", 1)[1].split("\n}\n\nif ! wait_for_runtime", 1)[0]
    assert 'docker exec "$CONTAINER" ps -eo pid,ppid,comm,args' in script
    assert 'docker top "$CONTAINER" -eo pid,comm,args' not in wait_check


def test_install_reloads_guardian_with_container_pid_namespace():
    script = (Path(__file__).parents[1] / "scripts" / "install.sh").read_text()
    reload_section = script.split("smoke=ok mode=auto", 1)[0]
    assert 'docker exec "$CONTAINER" ps -eo pid,ppid,comm,args' in reload_section
    assert 'guardian_pid=$(docker top "$CONTAINER"' not in script
    assert "ancestor_distance(candidate, launcher_pid)" in reload_section
    assert "kill -TERM \"$pid\"" in reload_section


def test_install_smoke_supports_socks_only_discovery():
    script = (Path(__file__).parents[1] / "scripts" / "install.sh").read_text()
    assert "socks5-hostname" in script
    assert 'case "$proxy_scheme"' in script


def test_build_steps_do_not_hardcode_a_mihomo_proxy_port():
    makefile = (Path(__file__).parents[1] / "Makefile").read_text()
    installer = (Path(__file__).parents[1] / "scripts" / "install.sh").read_text()
    assert "HTTPS_PROXY=http://127.0.0.1:7890" not in makefile
    assert "HTTP_PROXY=http://127.0.0.1:7890" not in makefile
    assert "HTTPS_PROXY=http://127.0.0.1:7890" not in installer
    assert "HTTP_PROXY=http://127.0.0.1:7890" not in installer


def test_install_does_not_swallow_rollback_failure_or_write_repo_temp_files():
    script = (Path(__file__).parents[1] / "scripts" / "install.sh").read_text()
    rollback_calls = [line for line in script.splitlines() if "scripts/rollback.sh" in line]
    assert rollback_calls
    assert all("|| true" not in line for line in rollback_calls)
    assert ".guardian.inspect.json" not in script
    assert ".guardian.discovery.json" not in script


def test_install_gates_legacy_single_file_mount_migration():
    script = (Path(__file__).parents[1] / "scripts" / "install.sh").read_text()
    assert "MIGRATE_BIN_MOUNT=0" in script
    assert "--migrate-bin-mount" in script
    assert "GUARDIAN_BIN_MOUNT_MODE" in script
    assert "GUARDIAN_BIN_SOURCE" in script
    assert "migration_required=1" in script
    assert "legacy-file" in script
    assert "maintenance window" in script


def test_install_detects_directory_and_legacy_guardian_destinations():
    script = (Path(__file__).parents[1] / "scripts" / "install.sh").read_text()
    assert "/guardian/bin/guardian" in script
    assert "/guardian/bin" in script
    assert "Mounts" in script
    assert "Mode" in script


def test_install_refuses_missing_guardian_binary_mount():
    script = (Path(__file__).parents[1] / "scripts" / "install.sh").read_text()
    assert 'GUARDIAN_BIN_MOUNT_MODE" = "missing"' in script
    assert "guardian binary mount is missing" in script


def test_install_backup_and_failure_trap_cover_guardian_binary():
    installer = (Path(__file__).parents[1] / "scripts" / "install.sh").read_text()
    rollback = (Path(__file__).parents[1] / "scripts" / "rollback.sh").read_text()
    assert "INSTALL_MUTATION_STARTED=0" in installer
    assert "trap on_install_exit EXIT" in installer
    assert installer.count("trap on_install_exit EXIT") == 1
    assert "trap cleanup EXIT INT TERM" not in installer
    assert "guardian_binary_hash" in installer
    assert "guardian_binary_present" in installer
    assert "start-guardian.sh" in installer
    assert "controller-secret" in installer
    assert "guardian_binary_hash" in rollback
    assert "guardian-binary" in rollback
    assert "manifest_artifact_is_valid guardian-binary" in rollback
    assert "guardian_launcher_present" in rollback
    assert "manifest_optional_artifact_is_valid guardian_secret_present controller-secret" in rollback
    assert "manifest_required_value compose" in rollback


def test_install_has_no_container_wide_stop_or_restart_commands():
    script = (Path(__file__).parents[1] / "scripts" / "install.sh").read_text()
    assert "docker stop" not in script
    assert "docker restart" not in script
    assert "docker kill" not in script
    assert "docker compose down" not in script


def _rollback_fixture(tmp_path: Path, docker_body: str):
    guardian_root = tmp_path / "guardian"
    backup_dir = guardian_root / "backups" / "chosen"
    project_dir = tmp_path / "project"
    fake_bin = tmp_path / "bin"
    backup_dir.mkdir(parents=True)
    (guardian_root / "run").mkdir()
    (guardian_root / "logs").mkdir()
    project_dir.mkdir()
    fake_bin.mkdir()

    compose = project_dir / "docker-compose.yml"
    config = tmp_path / "mihomo-config.yaml"
    compose.write_text("current-compose\n", encoding="utf-8")
    config.write_text("current-config\n", encoding="utf-8")
    (backup_dir / "compose.yml").write_text("backup-compose\n", encoding="utf-8")
    (backup_dir / "mihomo-config.yaml").write_text("backup-config\n", encoding="utf-8")
    (backup_dir / "manifest").write_text(
        "\n".join(
            (
                f"compose={compose}",
                f"project_dir={project_dir}",
                "service=mihomo",
                f"config_path={config}",
                "config_present=1",
            )
        )
        + "\n",
        encoding="utf-8",
    )
    docker = fake_bin / "docker"
    docker.write_text("#!/bin/sh\n" + docker_body, encoding="utf-8")
    docker.chmod(0o755)
    return guardian_root, backup_dir, compose, config, fake_bin


def test_rollback_restores_selected_backup_and_keeps_transaction_atomic(tmp_path):
    guardian_root, backup_dir, compose, config, fake_bin = _rollback_fixture(
        tmp_path,
        'if [ "$1" = compose ]; then exit 0; fi\n'
        'if [ "$1" = exec ]; then printf "COMMAND\\n mihomo\\n"; exit 0; fi\n'
        'if [ "$1" = inspect ]; then echo running; exit 0; fi\n'
        "exit 1\n",
    )
    script = Path(__file__).parents[1] / "scripts" / "rollback.sh"
    result = subprocess.run(
        ["sh", str(script), "--guardian-root", str(guardian_root), "--backup-dir", str(backup_dir)],
        env={"PATH": f"{fake_bin}:{os.environ['PATH']}"},
        text=True,
        capture_output=True,
    )
    assert result.returncode == 0, result.stderr
    assert compose.read_text(encoding="utf-8") == "backup-compose\n"
    assert config.read_text(encoding="utf-8") == "backup-config\n"


def test_rollback_restores_all_current_files_when_compose_validation_fails(tmp_path):
    guardian_root, backup_dir, compose, config, fake_bin = _rollback_fixture(
        tmp_path,
        'if [ "$1" = compose ] && [ "$4" = config ]; then exit 1; fi\n'
        'if [ "$1" = compose ]; then exit 0; fi\n'
        'if [ "$1" = inspect ]; then echo running; exit 0; fi\n'
        "exit 1\n",
    )
    script = Path(__file__).parents[1] / "scripts" / "rollback.sh"
    result = subprocess.run(
        ["sh", str(script), "--guardian-root", str(guardian_root), "--backup-dir", str(backup_dir)],
        env={"PATH": f"{fake_bin}:{os.environ['PATH']}"},
        text=True,
        capture_output=True,
    )
    assert result.returncode != 0
    assert compose.read_text(encoding="utf-8") == "current-compose\n"
    assert config.read_text(encoding="utf-8") == "current-config\n"


def test_rollback_uses_explicit_backup_and_shared_update_lock():
    script = (Path(__file__).parents[1] / "scripts" / "rollback.sh").read_text()
    assert 'backup_dir_arg=' in script
    assert 'if [ -n "$backup_dir_arg" ]' in script
    assert 'guardian-update.lock' in script
    assert 'flock -n 9' in script
    assert '--lock-held' in script


def test_rollback_validates_manifest_before_mutating_any_target():
    script = (Path(__file__).parents[1] / "scripts" / "rollback.sh").read_text()
    assert "manifest_is_valid" in script
    assert "backup is incomplete" in script
    assert "COMMIT_STARTED=1" in script
    assert "restore_previous_files" in script
    assert "RESTORE_IN_PROGRESS" in script

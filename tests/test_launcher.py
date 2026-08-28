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


def test_install_health_check_requests_pid_for_docker_top():
    script = (Path(__file__).parents[1] / "scripts" / "install.sh").read_text()
    wait_check = script.split("wait_for_runtime()", 1)[1].split("\n}\n\nif ! wait_for_runtime", 1)[0]
    assert 'docker top "$CONTAINER" -eo pid,comm,args' in wait_check


def test_install_smoke_supports_socks_only_discovery():
    script = (Path(__file__).parents[1] / "scripts" / "install.sh").read_text()
    assert "socks5-hostname" in script
    assert 'case "$proxy_scheme"' in script


def test_install_does_not_swallow_rollback_failure_or_write_repo_temp_files():
    script = (Path(__file__).parents[1] / "scripts" / "install.sh").read_text()
    rollback_calls = [line for line in script.splitlines() if "scripts/rollback.sh" in line]
    assert rollback_calls
    assert all("|| true" not in line for line in rollback_calls)
    assert ".guardian.inspect.json" not in script
    assert ".guardian.discovery.json" not in script

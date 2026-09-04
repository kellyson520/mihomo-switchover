from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]


def _read(relative: str) -> str:
    return (ROOT / relative).read_text(encoding="utf-8")


def test_agent_entrypoint_points_to_production_docs():
    text = _read("AGENTS.md")
    assert "skills/mihomo-guardian-production/SKILL.md" in text
    assert "docs/configuration.md" in text
    assert "号池" in text


def test_configuration_guide_covers_safe_single_file_operations():
    text = _read("docs/configuration.md")
    required = (
        "/opt/mihomo-cliproxy/guardian/guardian.yaml",
        "热重载",
        "回滚",
        "decision",
        "probes",
        "purity",
        "mihomo.proxy",
        "200–499",
        "providers/proxies",
        "minimum_coverage_percent",
        "purity.sources",
        "baseline",
    )
    for marker in required:
        assert marker in text, marker


def test_production_skill_has_valid_frontmatter_and_safety_contract():
    text = _read("skills/mihomo-guardian-production/SKILL.md")
    assert text.startswith("---\n")
    frontmatter, body = text.split("\n---\n", 1)
    fields = dict(
        line.split(":", 1)
        for line in frontmatter.splitlines()[1:]
        if ":" in line
    )
    assert fields["name"].strip() == "mihomo-guardian-production"
    assert fields["description"].strip().startswith("Use when")
    for marker in (
        "guardian 崩溃不得停止 mihomo",
        "status.sh --read-only",
        "install.sh --preflight",
        "guardian.yaml",
        "observe",
        "auto",
        "rollback.sh",
        "不得绕过 mihomo",
    ):
        assert marker in body, marker
    assert "TBD" not in body
    assert "TODO" not in body


def test_readme_links_to_new_operator_documents():
    text = _read("README.md")
    assert "docs/configuration.md" in text
    assert "skills/mihomo-guardian-production/SKILL.md" in text


def test_installer_and_rollback_preserve_quality_state_and_logs():
    installer = _read("scripts/install.sh")
    rollback = _read("scripts/rollback.sh")
    for marker in (
        "quality-store",
        "quality.jsonl",
        "quality_store_present",
        "quality_log_present",
    ):
        assert marker in installer, marker
    for marker in (
        "rollback-preserved-",
        "quality-store",
        "quality history/logs were retained",
    ):
        assert marker in rollback, marker


def test_quality_cli_and_status_are_documented_without_secret_output():
    readme = _read("README.md")
    skill = _read("skills/mihomo-guardian-production/SKILL.md")
    main = _read("cmd/guardian/main.go")
    for marker in (
        "quality run",
        "quality status",
        "quality baseline-reset",
        "daemon_running",
        "latest_quality_score",
    ):
        assert marker in main, marker
    for marker in ("quality status", "baseline-reset", "minimum_coverage_percent", "quality.jsonl"):
        assert marker in skill, marker
    assert "quality" in readme


def test_update_flow_documents_migration_and_guardian_only_daily_updates():
    documents = (
        _read("README.md"),
        _read("docs/configuration.md"),
        _read("skills/mihomo-guardian-production/SKILL.md"),
    )
    required = (
        "update-guardian.sh",
        "--preflight",
        "--migrate-bin-mount",
        "migration_required=1",
        "/guardian/bin:/guardian/bin",
        "原子",
        "guardian/quality",
        "维护窗口",
        "日常更新",
        "update_rollback_failed",
        "guardian-update.lock",
        "代理监听",
        "容器内 ps",
    )
    for document in documents:
        for marker in required:
            assert marker in document, marker

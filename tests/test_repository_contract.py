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

# Production documentation and agent skill plan

## Objective

把 mihomo-guardian 的生产投入约束、单文件配置说明、热重载边界和安全回滚流程固定
在仓库中，让后续 Agent 能在不影响 mihomo 生产代理的前提下快速完成部署和排障。

## Deliverables

- `AGENTS.md`: 仓库入口规则、安全边界和必须阅读的生产资料。
- `docs/configuration.md`: `/guardian/guardian.yaml` 字段、默认值、探测判定、provider
  粘性、热重载矩阵、配置修改与回滚。
- `skills/mihomo-guardian-production/SKILL.md`: 生产投入 Agent Skill，包含只读预检、
  observe 到 auto、验收、故障处置和回滚命令。
- `tests/test_repository_contract.py`: 文档/Skill 的最小结构契约。
- `README.md`: 新文档入口。
- `.gitignore`: 忽略 Python 测试缓存。

## Verification contract

- 契约测试先在文件缺失时失败，再在文件完成后通过。
- `pytest -q`、`make check CONTAINER=mihomo-cliproxy`、`make build CONTAINER=mihomo-cliproxy`。
- `git diff --check` 和 `file dist/guardian`。
- 提交前检查 Git 暂存内容，不包含 secret、日志、状态、构建产物或缓存。
- 推送后核对 `refs/heads/main` 与本地 HEAD 一致。

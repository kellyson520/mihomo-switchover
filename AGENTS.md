# Agent 工作约定

这是一个注入现有 mihomo 容器的独立 guardian 项目。号池在 mihomo 容器之外，
不要把号池、账号、额度、登录态或业务数据当作本项目的可修改对象。

## 开始工作前

必须先阅读：

1. [`skills/mihomo-guardian-production/SKILL.md`](skills/mihomo-guardian-production/SKILL.md)
2. [`docs/configuration.md`](docs/configuration.md)

Skill 是生产投入、验收、回滚和配置修改的安全门；配置文档是唯一行为配置文件的
字段说明。若本次任务涉及代码实现，还要先读相关设计/计划，并在提交前完成对应验证。

## 生产安全边界

- guardian 是旁路守护进程；guardian 崩溃、配置错误或 mihomo API 暂时失联时，不能
  停止 mihomo，也不能向 mihomo PID 发送信号。
- 禁止用 `docker stop`、`docker kill`、`docker compose down` 修复 guardian；只允许
  由 launcher 重启 guardian。必要的容器重建必须经过备份、预检和回滚路径。
- 控制 API 只直连容器内 loopback；所有公网探测必须经 mihomo 代理，不得绕过 mihomo。
- 生产行为配置只修改挂载区的 `guardian.yaml`，不要把 secret、API key、状态文件或
  日志提交到仓库。
- 任何声称“已完成”“测试通过”或“已投入生产”之前，必须运行新鲜的验证命令并记录
  结果；不能用推测代替验证。

## 变更和交付

先做只读状态检查和 `install.sh --preflight`，再做任何生产写入。配置修改遵循配置文档
的备份、校验、热重载、观察、验收和回滚流程。提交/推送前检查 `git diff --check`、
项目测试、静态构建和仓库内容，确认没有 secret、日志、状态或测试缓存。

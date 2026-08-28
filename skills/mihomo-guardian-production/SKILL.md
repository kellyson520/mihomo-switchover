---
name: mihomo-guardian-production
description: Use when deploying, configuring, validating, monitoring, or rolling back mihomo-guardian in a production mihomo container
---

# Mihomo Guardian Production

## 目的

本 Skill 用于把 guardian 安全地注入现有 mihomo 容器、修改唯一行为配置、验收主备
切换和处理故障。号池在容器外；本 Skill 不读取、不修改号池、账号、额度、登录态或
业务数据。

开始前必须读取：

- `docs/configuration.md`
- `configs/guardian.example.yaml`
- `README.md`

## 不可违反的生产规则

- guardian 崩溃不得停止 mihomo。launcher 只重启 guardian；不要给 mihomo PID 发信号。
- 不执行 `docker stop mihomo-cliproxy`、`docker kill mihomo-cliproxy`、
  `docker compose down` 来修 guardian。
- mihomo 控制 API 只直连容器内 loopback；公网探测、纯净度探测、厂商入口探测不得绕过 mihomo，
  必须走 `mihomo.proxy`。
- 只把 `/guardian/guardian.yaml` 当作生产行为配置；不要编辑仓库模板来代替挂载区文件。
- 不把 secret、API key、订阅 token、状态 JSON 或日志写入仓库、终端回显或提交。
- provider 没有 `alive` 和非空健康 `history` 时保持 fail-closed；不得为了“快速切换”
  把未经验证的节点切入生产。
- 纯净度只用于告警/评分，不能单凭一次纯净度结果切换渠道。

## 阶段 0：只读发现

先执行并保存输出：

```sh
sudo ./scripts/status.sh --read-only
sudo ./scripts/install.sh --preflight
```

预检必须确认：容器名、Compose 服务、mihomo 配置挂载、控制器 secret、loopback API、
loopback 代理端口、`CHANNEL/MAIN/BACKUP` 组和 provider 映射均唯一。发现不唯一时停止，
使用 `--container` 或 `--compose` 明确指定；不要猜端口、IP、组名或挂载路径。

## 配置修改办法

生产只改一个文件：

```text
/opt/mihomo-cliproxy/guardian/guardian.yaml
```

实际根目录以 `status.sh --read-only` 输出为准。修改顺序固定为：

1. 读取 `docs/configuration.md` 的字段和热重载矩阵。
2. 备份 `/guardian/guardian.yaml`，只改一个字段组，绝不写入 secret。
3. 用 `guardian reload --config /guardian/guardian.yaml` 做解析/校验。
4. 查看 `config_reloaded` 或 `config_reload_failed`，再运行只读状态和探测。
5. 首次部署、探测变化或切换策略变化先用 `decision.mode: observe`。
6. 多个周期确认合理后，才执行 `guardian auto` 解除人工强制状态。

示例：

```sh
docker exec mihomo-cliproxy sh -c \
  'cp -p /guardian/guardian.yaml /guardian/guardian.yaml.bak'
# 编辑实际挂载区的 guardian.yaml
docker exec mihomo-cliproxy /guardian/bin/guardian reload \
  --config /guardian/guardian.yaml
docker exec mihomo-cliproxy /guardian/bin/guardian probe \
  --config /guardian/guardian.yaml
sudo ./scripts/status.sh --read-only
```

可热重载：`decision.mode`、决策阈值/周期/保持时间、`probes`、`purity`、
`reload.check_interval`。需要重启 guardian：`mihomo` 连接端点、secret 路径、组、
provider、日志配置、`link_loss_grace`、`startup_api_timeout`。静态字段变更后不能
假定已生效；只重启 guardian 子进程，禁止重启 mihomo。

非法配置应继续使用上一份有效配置。若配置错误导致 guardian 反复退出，恢复备份并
检查日志；不要为了恢复 guardian 停止 mihomo。

## 安全部署流程

代码变更在宿主机先验证：

```sh
git diff --check
pytest -q
make check CONTAINER=mihomo-cliproxy
make build CONTAINER=mihomo-cliproxy
file dist/guardian
```

首次或重新注入时：

```sh
sudo ./scripts/install.sh --preflight
sudo ./scripts/install.sh --observe
```

确认 `mihomo` 和 `guardian` 都在运行、代理端口可用、OpenAI/Gemini 返回可接受的
`200–499`（401/403 代表入口可达，不代表账号认证成功），并检查日志后，才允许：

```sh
docker exec mihomo-cliproxy /guardian/bin/guardian auto \
  --config /guardian/guardian.yaml
```

不要在 `observe` 验收前启用自动切换。安装器会备份 Compose、mihomo 配置、旧切换器、
状态和 guardian 配置；预检失败时不得继续写入。

## 验收清单

```sh
sudo ./scripts/status.sh --read-only
docker top mihomo-cliproxy -eo pid,ppid,comm,args
docker exec mihomo-cliproxy /guardian/bin/guardian status \
  --config /guardian/guardian.yaml
docker exec mihomo-cliproxy /guardian/bin/guardian probe \
  --config /guardian/guardian.yaml
docker exec mihomo-cliproxy sh -c \
  'tail -n 100 /guardian/logs/guardian.jsonl'
```

必须看到：

- mihomo 进程和代理端口仍然工作；
- guardian 与 mihomo 是独立进程；
- 当前渠道只能是配置的 main 或 backup；
- 候选 provider 节点有 `alive` 和健康历史；
- 节点锁已写入 `/guardian/data/state.json`，健康节点不会无故改变；
- 日志中有探测分类、provider 验证和决策原因，且没有 secret；
- guardian 故障时 launcher 能重新拉起 guardian，mihomo 不被停止。

## 监控、人工切换和回滚

优先查看 JSONL 日志、状态 JSON 和 provider 元数据。不要用公网直连命令验证线路。
人工切换前要确认目标节点已验证：

```sh
docker exec mihomo-cliproxy /guardian/bin/guardian switch backup \
  --config /guardian/guardian.yaml
```

人工切换会写审计日志并有保护期；处理完必须：

```sh
docker exec mihomo-cliproxy /guardian/bin/guardian auto \
  --config /guardian/guardian.yaml
```

仅配置改错：恢复备份文件、重新 `reload`、观察日志。注入/Compose/mihomo 配置变更：

```sh
sudo ./scripts/rollback.sh --guardian-root /opt/mihomo-cliproxy/guardian
```

此回滚会重建 Compose 服务，可能短暂重启 mihomo，必须确认维护窗口；它是受控回滚路径，
不能用 `docker compose down` 替代。回滚后再次执行只读状态和完整验收。

## 常见错误

- 把宿主发布的 `7891` 当成容器内代理：使用发现结果，容器内通常是
  `127.0.0.1:<mixed/http/socks port>`。
- 把 401/403 当成线路失败：默认 `200–499` 都是入口可达。
- provider 没有历史仍强行选节点：保持 fail-closed，等待 mihomo provider 健康数据。
- 直接切换到延迟最低节点：优先持久化锁定的健康节点，避免出口频繁变化。
- 修改 `mihomo.api`/组/provider 后只执行 reload：这些字段需要 guardian 重启或重新走
  注入流程。
- 用 `docker restart` 或停止整个容器修复 guardian：这会扩大故障面，先只处理 guardian。

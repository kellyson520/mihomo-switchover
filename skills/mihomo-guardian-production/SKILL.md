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

## 重新注入与更新门禁

先区分“一次性挂载迁移”和“日常二进制更新”，不要把普通更新走成容器重建：

```sh
sudo ./scripts/install.sh --preflight
sudo ./scripts/update-guardian.sh --preflight
```

若任一预检报告 `migration_required=1`，当前仍是旧的单文件 guardian 挂载。必须先确认
维护窗口，再显式执行：

```sh
sudo ./scripts/install.sh --migrate-bin-mount --observe
```

这次迁移将 `/guardian/bin/guardian` 改为持久化目录挂载
`/opt/mihomo-cliproxy/guardian/bin:/guardian/bin:ro`，可能重建 Compose 服务并短暂影响
Mihomo；安装器会先备份、验证 Compose 和 mihomo 配置，失败时使用备份回滚。观察模式验收
Mihomo PID、代理端口、guardian 进程、OpenAI/Gemini 探测和日志后，才恢复 `auto`。

目录挂载完成后，日常更新只能使用：

```sh
sudo ./scripts/update-guardian.sh --preflight
sudo ./scripts/update-guardian.sh --observe
```

更新器验证新 ELF/hash，在宿主持久化目录内备份并原子 rename，只 TERM guardian/quality
子进程并等待 launcher 拉起新版本；不会重建、停止或重启 Mihomo，也不修改 Mihomo 配置、
provider、代理组、状态或质量 store。验证失败只恢复旧二进制并记录
`update_rolled_back`。更新日志为 `guardian/logs/guardian-update.jsonl`，不得包含 secret、
API key、订阅 URL 或账号信息。普通日常更新不需要维护窗口；若仍看到
`migration_required=1`，停止并安排一次迁移，不能绕过门禁。

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

### 公网探测频率

`decision.interval` 是 guardian 主循环周期，默认 `15s`；`decision.probe_interval` 是
OpenAI、Gemini 等公网厂商以及纯净度查询的最短刷新间隔，默认 `5m`。同一渠道/节点在
缓存期间不重复访问公网，切换到新节点会立即生成一次新样本。连续失败计数只对新样本
递增，因此不会把缓存中的同一次失败误判为多次故障。

日志 watcher 订阅 mihomo 本地 `/logs?level=error` WebSocket；拨号、TCP、TLS、超时、
重置等网络错误只打破当前探测缓存，随后按 `failure_recheck_interval`（默认 `30s`）
做有限次数的关键厂商复核。正常探测、mihomo 日志提示和月度质量巡检是三类独立证据，
任何一类都不能单独切换渠道。

生产建议保持 `probe_interval: 5m` 或更长，并通过 mihomo provider 的 `alive` 和
history 判断节点候选；不要把 `decision.interval` 改成 5 分钟来代替它，否则会同时拖慢
主备决策循环。provider 候选未验证时，guardian 会通过 mihomo loopback API 请求该
provider 的原生 `/healthcheck`，按独立的 `recovery_healthcheck_interval` 限频；这是异步健康检查，不
选择 `CHANNEL`，不会让备用流量短暂经过主渠道。请求失败继续 fail-closed。日志会出现
`provider_healthcheck_requested` 或 `provider_healthcheck_failed`。修改后按上面的
`reload` 流程校验，先在 `observe` 模式观察日志。

### Gemini 地区锁定与双厂商候选

Gemini 返回 `400 User location is not supported` 时，不能按普通 400/403 入口响应处理。
在对应探测下增加显式拒绝模式，例如：

```yaml
- id: gemini
  url: https://generativelanguage.googleapis.com/v1beta/models
  critical: true
  reject_body_patterns:
    - '(?i)user\s+location.{0,120}(not\s+supported|unsupported|not\s+available|unavailable)'
    - '(?i)service.{0,120}(not\s+available|unavailable).{0,120}(country|region)'
```

命中后记录 `route_policy_error`，guardian 仅从同一 provider 的 `alive + history` 节点中
逐个复核，并要求所有启用的 `critical` 探测通过。配置了 OpenAI 和 Gemini 为 critical 时，
候选必须同时通过两者才会被固定；成功后只写对应 provider 组，不直接改 `CHANNEL`。候选
全部失败时恢复原节点并保持现有 fail-closed 决策。响应体不写入日志，模式错误会在热重载
时拒绝并继续使用上一份有效配置。

### Quality 目标配置

`quality` 是同一份 `guardian.yaml` 中的可选配置。旧部署可省略它，或保持
`enabled: false`；不要为了启用质量扫描去修改号池配置。目标名称、来源组、provider、
过滤器和扫描顺序完全由用户填写，不存在必须叫 `MAIN`、`BACKUP-USA` 或某个地区名称的
约定。例如：

```yaml
quality:
  enabled: true
  full_scan_interval: 720h
  retry_interval: 24h
  order: [primary, reserve]
  targets:
    - id: primary
      source_group: YOUR_PRIMARY_GROUP
      provider: YOUR_PRIMARY_PROVIDER
      scope: locked
      lock_key: main
      listener: http://127.0.0.1:17990
    - id: reserve
      source_group: YOUR_RESERVE_GROUP
      provider: YOUR_RESERVE_PROVIDER
      scope: all
      node_filter: "YOUR_REGION_REGEX"
      listener: http://127.0.0.1:17991
```

修改时遵守以下契约：

- `order` 中的每个 target ID 必须在 `targets` 中恰好出现一次；不能重复、缺漏或引用不存在的 ID。
- ID 只能使用 `[a-z0-9][a-z0-9_-]{0,31}`；`source_group`、`listener` 必填。
- `scope: locked` 必须有 `lock_key`，只检查对应持久化锁定节点；`scope: all` 扫描来源组全部节点，再按可选 `node_filter` 筛选。
- listener 必须是唯一端口的 loopback HTTP 地址，并带显式端口；不要占用生产代理端口，也不要把质量 listener 指向公网地址。
- `node_filter` 必须是可编译正则。阈值、周期、保留期使用单文件中的默认值或按文档修改；配置加载会拒绝无效值。

保存后按既定流程执行 `guardian reload`，然后查看 `config_reloaded` 或
`config_reload_failed`。质量配置解析失败时继续使用上一份有效配置，不会停止 mihomo；
listener、隔离质量组等基础设施字段由安装器管理，不能仅靠手工改 URL 让它们生效。

质量运维命令（均在 mihomo 容器内执行）为：

```sh
/guardian/bin/guardian quality status \
  --config /guardian/guardian.yaml --data /guardian/data
/guardian/bin/guardian quality run \
  --config /guardian/guardian.yaml --data /guardian/data \
  --logs /guardian/logs --secret-file /guardian/controller_secret
/guardian/bin/guardian quality run --target TARGET_ID \
  --config /guardian/guardian.yaml --data /guardian/data \
  --logs /guardian/logs --secret-file /guardian/controller_secret
/guardian/bin/guardian quality baseline-reset \
  --config /guardian/guardian.yaml --data /guardian/data \
  --target TARGET_ID --node NODE_NAME --ip EXIT_IP
```

`quality status` 是只读命令，会显示 daemon、listener、扫描游标、报告数量、baseline
数量和最新质量/稳定性/综合分；不会打印 secret、订阅 URL、账号或请求凭证。`quality
run` 失败时不会停止 mihomo；daemon 由 launcher 独立监管。`baseline-reset` 只接受
target、精确节点名和合法 IP，保留旧报告并写入质量审计记录；不得把它当作常规换节点
手段。

质量评分规则：综合分 = 质量分 70% + 稳定性分 30%；稳定性内部是可用率 50%、P50
延迟 30%、抖动/峰值 20%，并按 coverage 折减。默认要求至少 3 个样本、
`minimum_coverage_percent: 10` 的 history
覆盖率且样本新鲜；不足时报告为 unknown/incomplete。IP/identity 与风险来源必须使用
显式稳定 ID，等价 URL 会去重。后续分数上涨只更新 latest/best，不提高 baseline；相对
初始 baseline 下降至少 20 分才解除 sticky。

质量扫描按 `quality.order` 固定顺序运行；mihomo 原生健康检测继续提供 provider
health，guardian 每小时汇总已有延迟 history。首次完整有效报告建立不可变的
`baseline_score`；后续分数上涨只更新 latest/best/历史，不自动改 baseline 或替换当前
固定节点。身份包含 target、provider、节点名和 IP，换 IP 会产生新身份和新 baseline，
旧身份历史保留。默认只有相对初始 baseline 下降至少 20 分才解除粘性，并且仍需通过
连通性、provider history、IP 一致性和置信度复核。

稳定性汇总是质量 daemon 的独立只读阶段：它只读取 provider 已有 history，不调用
`/delay`、不发起公网请求、不调用 `SetProxy`。它按 `quality.order` 和每个 target 的
`scope/node_filter` 遍历，只有已有完整 IP 身份的节点才更新 `stability.json`、
`stability-history.jsonl` 和 latest 推荐输入；未建身份的节点等待下一次全量扫描，不会
生成无 IP 记录。`quality status` 的 `latest_stability_at` 用来区分小时汇总时间和全量
质量报告时间。汇总失败会记入 `quality_stability_summary_failed` 并按更短周期重试；
失去 mihomo 心跳只重启质量子进程，不停止 mihomo 或实时 guardian。三重保险的顺序是：
正常状态低频公网探测、mihomo 网络错误日志触发的快速复核、以及按月的全量质量巡检；
其中日志提示和质量评分都不能绕过关键厂商 quorum、provider `alive/history` 和粘性节点保护。

可热重载：`decision.mode`、决策阈值/周期/保持时间、`decision.probe_interval`、
`decision.failure_recheck_interval`、`decision.recovery_healthcheck_interval`、`probes`、`purity`、
`quality`（目标顺序、阈值、周期、保留期）、`reload.check_interval`。需要重启 guardian：`mihomo` 连接端点、secret 路径、组、
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
`200–499`（未命中拒绝模式时，401/403 代表入口可达，不代表账号认证成功），并检查日志后，才允许：

```sh
docker exec mihomo-cliproxy /guardian/bin/guardian auto \
  --config /guardian/guardian.yaml
```

不要在 `observe` 验收前启用自动切换。安装器会备份 Compose、mihomo 配置、旧切换器、
状态、guardian 配置、质量 store（reports、baselines、scan progress、audit）和
`quality.jsonl`；预检失败时不得继续写入。

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
回滚会先把当前质量 store 和质量日志保存到 `backups/rollback-preserved-*`，然后恢复
部署文件；不会删除或覆盖当前质量历史，便于排查回滚前后的分数变化。

## 常见错误

- 把宿主发布的 `7891` 当成容器内代理：使用发现结果，容器内通常是
  `127.0.0.1:<mixed/http/socks port>`。
- 把未命中拒绝模式的 401/403 当成线路失败：默认 `200–499` 都是入口可达。
- provider 没有历史仍强行选节点：保持 fail-closed，等待 mihomo provider 健康数据。
- 直接切换到延迟最低节点：优先持久化锁定的健康节点，避免出口频繁变化。
- 修改 `mihomo.api`/组/provider 后只执行 reload：这些字段需要 guardian 重启或重新走
  注入流程。
- 用 `docker restart` 或停止整个容器修复 guardian：这会扩大故障面，先只处理 guardian。

# Guardian 配置说明

本文档描述注入 `mihomo-cliproxy` 后的唯一行为配置文件。号池不在 mihomo 容器
内，guardian 不读取或修改号池业务。

## 配置位置和职责

生产文件是宿主机挂载区中的：

```text
/opt/mihomo-cliproxy/guardian/guardian.yaml
```

实际路径由安装器根据 Compose 和 Docker 挂载自动发现，若部署路径不同，以
`docker inspect` 或 `status.sh --read-only` 显示的 `guardian_root` 为准。仓库中的
[`configs/guardian.example.yaml`](../configs/guardian.example.yaml) 只是模板，不是
生产运行文件。

只编辑这一份 `guardian.yaml`。安装器会从 mihomo 配置发现容器内 API、代理端口、
主备组和 provider，并只重写配置中的基础设施段；`decision`、`probes`、`purity`、
`quality`、`logging` 和 `reload` 等行为段由模板/现有配置保留。

不要在文件中写入控制器 secret、厂商 API key、订阅 token 或账号信息。控制器 secret
应保存在挂载区的：

```text
/opt/mihomo-cliproxy/guardian/controller_secret
```

权限应限制为 guardian 可读（通常 `0640`）。日志会对 URL 查询参数中的 token、secret、
key 和 password 做脱敏，但不要把脱敏当作可以记录 secret 的理由。

## 完整配置骨架

```yaml
mihomo:
  api: http://127.0.0.1:9090
  proxy: http://127.0.0.1:7890
  secret_file: /guardian/controller_secret

groups:
  channel: CHANNEL
  main: MAIN
  backup: BACKUP-USA

providers:
  main: main-channel
  backup: backup-channel

purity:
  enabled: true
  automatic_switch: false
  sources:
    - id: identity-a
      url: https://YOUR-IP-SOURCE-A.example/ip
      kind: identity
      format: text
    - id: identity-b
      url: https://YOUR-IP-SOURCE-B.example/json
      kind: identity
      format: json
    - id: risk-a
      url: https://YOUR-RISK-SOURCE-A.example/check
      kind: risk
      format: json
    - id: risk-b
      url: https://YOUR-RISK-SOURCE-B.example/check
      kind: risk
      format: json

# 目标 ID、顺序、来源组和过滤器均由用户定义；下面的名称只是示例。
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
  per_node_timeout: 180s
  thresholds:
    baseline_drop_points: 20
    minimum_confidence: 70
    candidate_minimum_score: 60
    recovery_margin_points: 10
    recovery_confirmations: 2
  stability:
    summary_interval: 1h
    history_window: 24h
    minimum_samples: 3
    minimum_coverage_percent: 10
    stale_after: 26h
    good_latency_ms: 500
    bad_latency_ms: 3000
  retention:
    reports: 90
    history_days: 180

decision:
  mode: observe
  interval: 15s
  probe_interval: 5m
  failures_before_switch: 3
  recoveries_before_switch: 2
  min_hold: 120s
  link_loss_grace: 15s
  startup_api_timeout: 60s
  critical_quorum: 2

probes:
  - id: openai
    url: https://api.openai.com/v1/models
    critical: true
    enabled: true
    method: GET
    timeout: 5s
    delay_timeout: 5s
    expected_min: 200
    expected_max: 499
  - id: gemini
    url: https://generativelanguage.googleapis.com/v1beta/models
    critical: true

logging:
  max_bytes: 10485760
  retain: 7

reload:
  check_interval: 2s
```

上面的组名和 provider 名称是示例占位符；首次部署应让安装器自动发现并替换，不能
直接把占位符当作线上实际名称。

持续运行时，guardian 在 mihomo API 心跳成功后检查配置文件。完整解析和校验成功才
会替换内存配置；错误文件继续使用上一份有效配置，并写入
`config_reload_failed`。有效热重载写入 `config_reloaded`。

## 字段说明

### `mihomo`

| 字段 | 说明 |
| --- | --- |
| `api` | mihomo 控制 API，只允许容器内 loopback 的 HTTP URL，例如 `http://127.0.0.1:9090`。guardian 直接访问，不走公网代理。 |
| `proxy` | 公网探测出口，只允许 loopback 的 HTTP/HTTPS/SOCKS5 URL。所有 OpenAI、Gemini、Anthropic、OpenRouter、DeepSeek 和纯净度请求都必须走这里。 |
| `secret_file` | 控制 API secret 的容器内文件路径。默认命令参数是 `/guardian/controller_secret`；改此字段需要重启 guardian。 |

### `groups`

| 字段 | 说明 |
| --- | --- |
| `channel` | 实际承载流量的主备选择器，例如 `CHANNEL`。guardian 只允许它当前选择 `main` 或 `backup`。 |
| `main` | 主渠道的 mihomo proxy group 名称。 |
| `backup` | 备用渠道的 mihomo proxy group 名称。 |

组名必须与 mihomo 控制 API 返回的 `/proxies` 完全一致。修改组关系不是普通热重载，
需要重新部署/重启 guardian；改错时 guardian 会拒绝自动决策。

### `providers`

`main` 和 `backup` 必须同时填写或同时留空。填写时值是 mihomo 的 provider 名称，
不是订阅 URL。安装器从 `proxy-groups.<name>.use` 自动发现并填充。

guardian 使用：

```text
GET /providers/proxies/<provider>
```

作为节点健康依据。候选节点必须在 provider 返回结果中存在、`alive: true`，并且有
非空 `history`；没有健康历史的节点保持未知并拒绝切换（fail-closed）。provider
元数据不可用时，不会退回“猜测一个节点”。

每个 provider 的上次成功节点写入持久化状态：

```text
/opt/mihomo-cliproxy/guardian/data/state.json
```

下次检查优先复用该节点；节点仍健康时不因一次延迟波动随机更换，从而降低出口变化
和号池风控风险。若没有 provider 配置，guardian 才使用 mihomo 的节点 delay API
验证候选；该检查不会把候选临时切入生产流量。

### `quality`

这是可选的质量扫描契约。旧部署可以省略整个 `quality` 段，或明确设置
`enabled: false`；启用时至少需要一个 target。target 的 ID、来源分组、provider、
扫描顺序和过滤器都由用户配置，没有固定的 `MAIN`、`BACKUP-USA`、地区名称或 target
名称要求。

`order` 是严格的扫描顺序；其中每个 ID 必须在 `targets` 中恰好出现一次，不能重复或
缺漏。target ID 必须匹配 `[a-z0-9][a-z0-9_-]{0,31}`。每个 target 的
`source_group` 和 `listener` 必填，`listener` 必须是带明确端口的 loopback HTTP URL，
且端口不能重复。`node_filter` 是用户自定义正则，guardian 会在加载配置时编译它，
无法编译的配置会被拒绝。

| 字段 | 说明 |
| --- | --- |
| `source_group` | mihomo 中待扫描的来源 proxy group，名称必须与控制 API 返回值一致。 |
| `provider` | 可选的 mihomo provider 名称；留空时由运行时按来源组解析静态节点。 |
| `scope: locked` | 只扫描持久化状态中 `lock_key` 对应的当前锁定节点；必须填写 `lock_key`。 |
| `scope: all` | 扫描来源组中的全部节点，并可用 `node_filter` 进一步筛选。 |
| `listener` | 质量扫描专用的 loopback listener；不能指向生产代理端口。 |

默认时间和阈值如下：

| 配置 | 默认值 | 作用 |
| --- | ---: | --- |
| `full_scan_interval` | `720h` | 全量质量扫描周期。 |
| `retry_interval` | `24h` | 失败目标重试间隔。 |
| `per_node_timeout` | `180s` | 单节点质量探测超时。 |
| `stability.summary_interval` | `1h` | 每小时汇总 mihomo history。 |
| `stability.history_window` / `stale_after` | `24h` / `26h` | 稳定性统计窗口和过期界线。 |
| `stability.minimum_samples` | `3` | 稳定性结论所需的最小样本数。 |
| `stability.minimum_coverage_percent` | `10` | history 覆盖预期采样窗口不足时不建立稳定性结论，避免少量样本伪装成全天稳定。 |
| `stability.good_latency_ms` / `bad_latency_ms` | `500` / `3000` | 延迟评分区间，后者必须大于前者。 |
| `thresholds.baseline_drop_points` | `20` | 相对初始 baseline 的释放阈值，单位为分。 |
| `thresholds.minimum_confidence` / `candidate_minimum_score` | `70` / `60` | 推荐所需的最低置信度和候选分数。 |
| `thresholds.recovery_margin_points` / `recovery_confirmations` | `10` / `2` | 恢复候选的安全余量和确认次数。 |
| `retention.reports` / `history_days` | `90` / `180` | 报告数量和 history 保留天数。 |

质量分使用 IP/厂商/风险证据，稳定性分使用 mihomo provider 的延迟 history；最终分按
质量 70%、稳定性 30% 合成。mihomo 自己的 provider 健康检测继续按其原生周期运行，
guardian 每小时读取并汇总已有 history，不为汇总额外制造公网探测。history 过期、样本
不足或覆盖率低于门槛时保持未验证并降低置信度，不把未知误判为失败或干净。稳定性
内部按可用率 50%、P50 延迟 30%、抖动/峰值 20% 计算；峰值仍会惩罚偶发严重延迟，且
最终稳定性分会按 coverage 再折减。

汇总只接受已经存在的 `target + provider + node + IP` 身份；新节点或尚未完成双来源 IP
确认的节点只记录 `quality_stability_identity_missing`，不会凭空创建无 IP 的记录或 baseline。
汇总会刷新该身份的 latest 稳定性字段和推荐输入，但不会写入来源组、`CHANNEL`，也不会
改变不可变 baseline。质量 daemon 与实时 guardian 相互独立，汇总失败只记录日志并按较短
的稳定性周期重试；mihomo API 心跳失败时质量进程退出，由 launcher 单独拉起，mihomo 不受影响。

第一次完整且有效的报告为该身份建立 `baseline_score`，baseline 不会因后续分数上涨而
改变。上涨只更新 `latest_score`、`best_score` 和历史记录；当前固定节点也不会因为另
一个节点分数更高而自动替换。身份是 `target + provider + node + IP`，IP 变化会创建
新的身份和新的 baseline，旧 IP 的历史保留，IP 恢复时可继续使用旧记录。

默认只有当前分数相对其初始 baseline 下降 20 分或更多（`baseline_drop_points: 20`）
才解除节点粘性；19 分下降不触发。解除前仍须有新鲜厂商连通性、provider 健康 history、
IP 身份一致和最低置信度。阈值、保留期和 target 顺序可热重载；listener、隔离质量组
等 mihomo 基础设施由安装器管理。

### `decision`

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `mode` | `auto` | `observe` 只记录应切换动作，`auto` 才执行自动切换。`force` 仅为兼容保留，日常生产不要设置；人工强制状态由 `switch` 命令写入状态文件。首次部署应使用 `observe`。 |
| `interval` | `15s` | guardian 主循环和 mihomo 本地健康检查周期；保持较短以便快速处理本地状态。 |
| `probe_interval` | `5m` | OpenAI、Gemini 等公网厂商探测及纯净度查询的最短刷新间隔。相同渠道/节点在缓存期间不重复访问公网；切换到新节点会立即刷新。 |
| `failures_before_switch` | `3` | 当前渠道连续失败次数达到该值才允许切换。 |
| `recoveries_before_switch` | `2` | 当前在备用渠道时，主渠道恢复确认所需的连续成功次数。 |
| `min_hold` | `120s` | 切换后的最短保持时间，防止来回抖动。 |
| `link_loss_grace` | `15s` | mihomo 控制 API 连续失联多久后 guardian 自身退出，由 launcher 仅重启 guardian。改动需要重启 guardian。 |
| `startup_api_timeout` | `60s` | guardian 启动时等待 mihomo API 的最长时间。改动需要重启 guardian。 |
| `critical_quorum` | `2` | 至少多少个启用的 critical 探测成功才算渠道健康，不能大于 critical 探测数。 |

主循环每 15 秒运行，但公网厂商探测默认每 5 分钟才刷新一次；缓存期间不会把同一次
失败重复计入连续失败次数。只有 3 次新的关键探测失败、恢复阈值、最短保持时间和候选
节点健康条件同时满足时才切换。纯净度评分不参与这个决策。

### `probes`

每个探测包含 `id`、`url`、`critical`，可选 `enabled`、`method`、`timeout`、
`delay_timeout`、`expected_min` 和 `expected_max`。默认方法为 `GET`，默认超时为
`5s`，默认可接受状态范围为 `200–499`。

探测分类规则是：

- `200–499`（包括常见的 `401`、`403`、`429`）表示厂商入口可达，不要求 API key；
- `5xx` 表示上游服务错误，本次失败；
- DNS、TCP、TLS、代理连接和超时错误表示本次失败；
- 其他不在期望范围内的状态记录为 `unexpected_http`。

因此，401/403 不能单独证明账号可用，也不能单独证明线路失效；它们只证明厂商入口
通过当前代理可达。调整 `expected_min/max` 前必须确认不会把认证错误误判成线路故障。

推荐至少保留 OpenAI 和 Gemini 两个 `critical: true` 探测；可按实际号池增加
Anthropic、OpenRouter、DeepSeek，但不要把单一厂商当成唯一切换依据。

所有这些请求由 guardian 的 HTTP 客户端显式使用 `mihomo.proxy`，不会因为宿主机的
`HTTP_PROXY`/`HTTPS_PROXY` 环境变量而绕过 mihomo。探测 URL 仍应使用 HTTPS。

### `purity`

启用后通过 mihomo 代理访问多个 IP/ASN 查询 URL，输出 `purity_advisory` 日志，包括
IP 冲突、国家冲突、数据中心 ASN 或 ASN 未知等告警和评分。

`automatic_switch` 当前只能保持 `false`：纯净度是辅助诊断信号，不是自动切换条件；
即使评分异常，也不会仅凭一次样本改变渠道。需要人工处理时，先查看多次日志、当前
provider 健康历史和厂商探测结果，再按运维命令执行有审计记录的人工切换。

`purity.urls` 是兼容旧配置的 IP/identity 简写，只能按默认规则推断格式。新配置建议
使用 `purity.sources`，显式写出稳定的 `id`、HTTPS `url`、`kind` 和 `format`：

| 字段 | 取值 | 说明 |
| --- | --- | --- |
| `id` | 稳定且唯一 | 作为独立来源身份；不能让两个别名指向同一端点来制造多数票。 |
| `kind` | `ip` / `identity` / `risk` | `risk` 来源进入风险评分；其他两类进入 IP/身份共识。 |
| `format` | `text` / `json` | text 必须只返回一个 IP；JSON 支持 IP、ASN、国家和 abuse/blacklist 等字段。 |
| `critical` | true/false | 记录运维重要性；不会绕过连通性或完整性门槛。 |

质量报告要求至少两个独立身份来源形成 IP 共识，并要求风险来源达到独立多数；缺少
显式风险来源时报告保持 `incomplete`，不能建立 baseline 或自动推荐。风险接口若需要
授权，授权应通过不落盘的外部注入机制提供，不能把 key/token 写入 `guardian.yaml`。
JSON 中明确存在但格式错误的高优先级 `ip` 字段会使该来源 fail-closed。等价 URL（主机
大小写、默认端口、查询参数顺序或 fragment 不同）会被规范化后去重。

质量只读状态和手工全量扫描：

```sh
docker exec mihomo-cliproxy /guardian/bin/guardian quality status \
  --config /guardian/guardian.yaml --data /guardian/data
docker exec mihomo-cliproxy /guardian/bin/guardian quality run \
  --config /guardian/guardian.yaml --data /guardian/data \
  --logs /guardian/logs --secret-file /guardian/controller_secret
```

`quality status` 不访问公网，也不选择节点；每个 target 同时显示全量报告时间
`latest_at` 和 history 汇总时间 `latest_stability_at`。`quality run` 仅扫描生成的
质量隔离组，失败不会停止 mihomo；常驻 quality daemon 会继续按配置周期运行。

### `logging` 与 `reload`

日志文件位于：

```text
/opt/mihomo-cliproxy/guardian/logs/guardian.jsonl
/opt/mihomo-cliproxy/guardian/logs/launcher.log
```

`logging.max_bytes` 控制 JSONL 轮转阈值，`logging.retain` 控制保留份数。两者需要
重启 guardian 才能让新的 logger 参数生效。`reload.check_interval` 控制配置文件检查
频率，可热重载。

常用事件包括 `probe`、`purity_advisory`、`node_verified`、`provider_unverified`、
`switch_observed`、`channel_switched`、`quality_stability_node_complete`、
`quality_stability_identity_missing`、`quality_stability_summary_failed`、
`config_reloaded` 和 `config_reload_failed`。日志中不应出现 secret 或 API key。

## 热重载矩阵

### 可热重载字段

修改合法文件后，以下字段会在下一个心跳/检查周期应用：

- `decision.mode`
- `decision.interval`
- `decision.probe_interval`
- `decision.failures_before_switch`
- `decision.recoveries_before_switch`
- `decision.min_hold`
- `decision.critical_quorum`
- `probes` 的内容和探测参数
- `purity` 的内容
- `quality` 的 target 顺序、扫描周期、阈值、稳定性窗口和保留期
- `reload.check_interval`

### 需要重启 guardian 的字段

以下字段属于已建立连接或进程级资源，修改后不能依赖热重载：

- `mihomo.api`
- `mihomo.proxy`
- `mihomo.secret_file`
- `groups`
- `providers`
- `quality` target 的 `listener` 及安装器生成的隔离质量组
- `logging`
- `decision.link_loss_grace`
- `decision.startup_api_timeout`

安全做法是只重启 guardian 子进程，让 launcher 自动拉起它；不要重启或停止 mihomo。
若改动涉及 Compose 挂载、mihomo 原配置或容器入口，则属于容器级变更，必须走安装器
的备份、预检和回滚流程。

## 配置修改标准流程

### 1. 先观察并备份

```sh
sudo ./scripts/status.sh --read-only
docker exec mihomo-cliproxy sh -c 'cp -p /guardian/guardian.yaml /guardian/guardian.yaml.bak'
```

如果生产根目录在宿主机可见，也可以在宿主机对实际挂载文件做带时间戳的备份。不要
覆盖 `guardian.yaml` 的临时写入过程；编辑器应写入同一挂载目录，并保持文件权限。

### 2. 只改一个配置文件并校验

先在仓库模板/临时副本中检查 YAML，再将变更同步到生产挂载区。生产校验命令：

```sh
docker exec mihomo-cliproxy /guardian/bin/guardian reload \
  --config /guardian/guardian.yaml
```

该命令只解析和校验，不会主动切换渠道；常驻 guardian 会在下一次检查时应用可热重载
字段。

### 3. 观察应用结果

```sh
docker exec mihomo-cliproxy sh -c \
  'tail -n 50 /guardian/logs/guardian.jsonl'
sudo ./scripts/status.sh --read-only
```

确认看到 `config_reloaded`，没有连续的 `config_reload_failed`，并检查当前渠道、主备
节点和 mihomo 进程仍在运行。首次部署或探测规则变化后，先保持：

```yaml
decision:
  mode: observe
```

观察到的切换理由合理、连续多个周期稳定后，才执行：

```sh
docker exec mihomo-cliproxy /guardian/bin/guardian auto \
  --config /guardian/guardian.yaml
```

注意：`auto` 清除人工强制状态；它不是容器重启，也不应停止 mihomo。

### 4. 配置失败时恢复

如果校验失败或日志出现错误，立即恢复备份，再次校验并观察：

```sh
docker exec mihomo-cliproxy sh -c \
  'cp -p /guardian/guardian.yaml.bak /guardian/guardian.yaml'
docker exec mihomo-cliproxy /guardian/bin/guardian reload \
  --config /guardian/guardian.yaml
```

如果只是配置热重载失败，旧配置仍在内存中，mihomo 不受影响。若 guardian 需要重新
启动，确认进程身份后只终止 guardian 子进程，等待 launcher 自动重启；不要给 mihomo
PID 发信号。

## 只读诊断与切换命令

```sh
sudo ./scripts/status.sh --read-only
docker exec mihomo-cliproxy /guardian/bin/guardian status --config /guardian/guardian.yaml
docker exec mihomo-cliproxy /guardian/bin/guardian probe --config /guardian/guardian.yaml
docker exec mihomo-cliproxy /guardian/bin/guardian switch backup --config /guardian/guardian.yaml
docker exec mihomo-cliproxy /guardian/bin/guardian auto --config /guardian/guardian.yaml
```

`status` 和 `probe` 是诊断命令；`switch` 是人工变更，会写入审计日志并产生 30 分钟
保护期。人工切换前要确认目标 provider 有 `alive` 和健康历史。`auto` 用于恢复自动
决策。

## 失败处置和回滚

guardian 的 API 失联、配置错误、探测异常或自身崩溃不应影响 mihomo；launcher 会仅
重启 guardian。先保存日志和状态，再处理根因：

```sh
docker exec mihomo-cliproxy sh -c \
  'tail -n 200 /guardian/logs/guardian.jsonl; cat /guardian/data/state.json'
```

安装器造成的 Compose、mihomo 原配置或挂载变更使用仓库回滚脚本：

```sh
sudo ./scripts/rollback.sh --guardian-root /opt/mihomo-cliproxy/guardian
```

该脚本会恢复最近的完整备份并重建目标 Compose 服务，因此属于容器级应急操作，可能
短暂重启 mihomo；执行前确认备份清单和业务维护窗口。它保留 guardian 的日志、状态和
备份目录，不删除排查证据。不要手工执行 `docker compose down`，不要 force push 或
删除备份目录。

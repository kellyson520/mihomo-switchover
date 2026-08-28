# IP Quality 与 mihomo 稳定性评分设计

**日期：** 2026-08-28  
**状态：** 待用户审阅  
**项目：** `/上传/mihomo-guardian`

## 1. 目标与边界

本设计为现有 mihomo guardian 增加独立的质量评估旁路。它的目标是对用户配置的锁定
目标和任意目标分组中的节点建立可追溯的 IP 质量、厂商可达性和 mihomo 延迟
稳定性基线，并让实时 guardian 在有充分证据时筛选合适节点。目标分组、provider、
节点范围和顺序都来自 `guardian.yaml`，不把 `MAIN`、`BACKUP-USA` 或“美国”写死。

质量评估不是号池，也不读取或修改账号、额度、登录态或业务数据。质量进程不直接写入
生产 `CHANNEL`、`MAIN` 或 `BACKUP-USA`。它只使用专用 mihomo 质量组、生成报告和
推荐文件；生产节点调整仍由实时 guardian 在重新校验证据后执行。

必须满足以下安全约束：

- 所有公网请求都经 mihomo 专用 loopback listener，不允许质量脚本直连公网；
- 质量扫描顺序固定为用户在配置中声明的目标顺序；
- 扫描一个节点失败、超时、脚本崩溃或质量服务不可用时，继续保存证据并进入下一个
  节点，不停止 mihomo、实时 guardian 或号池；
- 评分上涨只更新最新值和历史，不自动抬高初始基准；
- 节点出口 IP 改变时视为新的 IP 身份，不能直接沿用旧 IP 的基准；
- IP 质量只能在报告完整、置信度达标且存在合格候选时解除节点粘性；
- 连通性故障仍由实时 15 秒探测和现有主备决策处理，不能等待月度质量任务。

## 2. 方案选择

采用同容器内两个相互独立的 guardian 进程：

1. 现有 `guardian run` 继续负责实时厂商探测、主备决策和生产组写入；
2. 新增 `guardian quality-daemon`，负责小时稳定性汇总、月度全量质量扫描和报告持久化；
3. launcher 分别监督两个 guardian 子进程。任一 guardian 崩溃时只重启对应 guardian，
   不给 mihomo PID 发信号；
4. 两个进程通过原子状态、质量报告和推荐文件协作，不共享易损的内存状态；
5. 只有实时 guardian 能写入生产组和 `CHANNEL`。质量 daemon 只能写自动生成的
   `GUARDIAN-QUALITY-*` 隔离组和 `/guardian/data/ipquality/`。

不在生产循环中原样执行最新版 `xykt/IPQuality` 的 shell 脚本。该脚本依赖 Bash、curl、
jq、bc、dig、nc，部分路径可能绕过代理，并包含报告/统计外联。实现借鉴其多源聚合的
检测维度和风险字段，使用固定的 Go 适配器、固定请求白名单和 `-p` 语义，不动态下载
脚本、不携带 API key、不上传报告。若将来提供人工完整脚本模式，必须固定上游 commit、
校验 SHA256、单独记录 AGPL-3.0 许可，并且不允许进入自动月度任务。

## 3. mihomo 隔离入口

安装器为每个用户配置的质量目标增加一个不被生产组引用的 select 组和一个 loopback
listener。下面仅展示两个目标的示例，实际目标数量和名称由配置决定：

```yaml
proxy-groups:
  - name: GUARDIAN-QUALITY-primary
    type: select
    use:
      - provider-a
  - name: GUARDIAN-QUALITY-reserve-us
    type: select
    use:
      - provider-b

listeners:
  - name: guardian-quality-primary
    type: mixed
    listen: 127.0.0.1
    port: 17990
    proxy: GUARDIAN-QUALITY-primary
  - name: guardian-quality-reserve-us
    type: mixed
    listen: 127.0.0.1
    port: 17991
    proxy: GUARDIAN-QUALITY-reserve-us
```

端口不发布到宿主机，也不监听 `0.0.0.0`。上面的 `17990` 和 `17991` 只是端口选择示例；
安装器检查当前配置、mihomo API 和容器内监听表后，选择两个稳定的空闲端口并写入唯一
行为配置。端口、组名或 listener 能力无法唯一确认时预检失败，不修改 mihomo。

质量 daemon 每次扫描前从 guardian 状态读取目标的锁定节点，并只通过对应质量组选择
该节点。`source_group` 是用户要评估的现有 mihomo 分组，但质量 daemon 永远不向
`source_group` 写选择；它只写自动生成的隔离质量组。没有锁定记录、provider 元数据
没有 `alive: true` 和非空健康历史、节点已经从 provider 消失，或质量 listener 不可用
时，该节点标记为 `unverified`，不发起生产切换。

扫描质量组的选择不会改变 `MAIN`、`BACKUP-USA` 或 `CHANNEL`。安装器必须备份 mihomo
配置；若运行中的 Alpha 版本不支持独立 listener，则质量功能保持关闭，实时 guardian
继续按原配置运行，不得以降级方案临时切换 `CHANNEL`。

## 4. 扫描范围和调度

### 4.1 用户定义的固定顺序

每轮任务按照 `quality.targets` 的 `order` 字段执行，例如：

```text
primary 目标的当前锁定节点
        ↓
reserve-us 目标的 provider 原始顺序第 1 个节点
        ↓
reserve-us 目标的 provider 原始顺序第 2 个节点
        ↓
...
```

每个目标可以选择 `locked` 或 `all` 扫描范围。`locked` 只检测该目标的持久化锁定节点；
`all` 检测该目标 provider 或静态分组中的全部节点，并可由该目标自己的 `node_filter`
正则进一步筛选。分组和 provider 映射由安装器自动发现，也可以在配置中明确指定。
不按延迟随机排序、不并发扫描、不因为某个节点较快就改变 provider 原始顺序。

如果前一个目标检测失败，仍继续后续目标；如果某个节点失败，记录单节点失败后继续
下一个节点。单节点超时不阻塞整轮，整轮可在容器重启后依据游标继续。

### 4.2 月度全量质量任务

默认 `full_scan_interval: 720h`，以最近一次成功的完整扫描时间为基准，而不是以进程
启动时间为基准。扫描状态包含 provider 内容指纹、节点游标、最近尝试时间和最近成功
时间：

- 任一目标的 provider 或静态分组节点列表变化时，新节点立即进入本轮未扫描队列；
- 已消失节点保留历史，但标记 `provider_removed`；
- 失败节点不会被伪造为成功，默认 24 小时后重试；
- 前一个目标失败不阻止后续目标进入本轮；
- 扫描结果达到完整性要求后才更新该节点的月度成功时间。

### 4.3 每小时稳定性汇总

mihomo 的 provider 健康检测继续使用当前生产配置。当前配置为 300 秒探测一次；不把
它强行改成一小时，因为那会降低故障发现速度。guardian 每小时读取 mihomo 已产生的
延迟历史，计算滚动稳定性表，不重复对每个节点制造一套外部延迟请求。

稳定性窗口默认 24 小时，至少需要 3 个有效样本。历史样本过少、全部过期或无法区分
provider 未运行与节点失败时，稳定性为 `unknown`，不作为降分或换节点依据。

## 5. 分数模型

所有分数为 `0–100`，100 表示更好的质量或稳定性。最终分数为：

```text
effective_score = quality_score × 0.70 + stability_score × 0.30
```

### 5.1 IPQuality 质量分（70%）

| 子项 | 最终权重 | 规则 |
| --- | ---: | --- |
| 厂商入口连通性 | 30 | OpenAI、Gemini 和配置的其他厂商各执行至少两次；200–499 为入口可达，5xx、DNS、TCP、TLS 和超时失败 |
| IP/ASN/地区一致性 | 15 | 多个 IP 源、ASN、国家/地区和组织字段交叉比对；冲突降低分数和置信度 |
| 风险与黑名单 | 20 | Proxy、VPN、Tor、Hosting、滥用和黑名单结果按多源一致性合并；单个源不可用不等于干净或高风险 |
| 数据完整性与置信度 | 5 | 根据有效来源覆盖率、来源一致性和解析完整性计算；不足时报告为 incomplete |

IPQuality 适配器至少记录：出口 IP、IP 版本、ASN、组织/ISP、国家、数据中心/Hosting、
Proxy、VPN、Tor、黑名单/滥用状态、各来源原始状态、请求延迟和解析错误分类。报告中
不保存代理认证信息、控制器 secret 或完整查询凭据。

### 5.2 mihomo 稳定性分（30%）

稳定性分内部为：

| 子项 | 稳定性内部权重 | 规则 |
| --- | ---: | --- |
| 有效可用率 | 50 | 使用窗口内健康样本和期望采样次数计算；缺失采样先降低覆盖率，不直接判定节点坏 |
| 延迟表现 | 30 | 使用 p50/p95 延迟相对可配置的 good/bad 阈值归一化 |
| 延迟抖动 | 20 | 使用 p95-p50 和异常尖峰计算；单次尖峰不触发生产切换 |

稳定性记录至少包括样本数、覆盖率、最新样本时间、p50、p95、最大延迟、抖动、alive
状态、数据过期状态和 `stability_score`。稳定性数据源仍是 mihomo provider 的
`history`，不是质量脚本私自直连节点。

### 5.3 数据不足和硬门槛

质量分、稳定性分和最终分数同时带 `confidence_percent` 与 `complete` 字段：

- 任一关键数据源缺失时标记未知或 incomplete，不把未知替换成 0 或 100；
- `confidence_percent` 低于默认 70，报告只能告警，不能解除节点粘性；
- OpenAI/Gemini 关键连通性不满足 quorum 时，节点不能作为合格候选；
- 没有有效稳定性窗口时不能建立新的综合基准；
- 多源 IP 冲突时保存全部来源结果，并标记 `ip_conflict`。

## 6. 初始基准、分数上涨和 IP 身份

### 6.1 初始基准

节点第一次完成完整质量扫描、关键连通性通过、置信度达标且稳定性样本达到最低数量
后，建立：

```text
baseline_score
baseline_quality_score
baseline_stability_score
baseline_ip
baseline_created_at
```

基准属于 `provider + 节点名 + IP 版本 + 出口 IP` 这一个身份，不能因为后续分数上涨
而自动改写。

### 6.2 分数上涨

分数上涨时必须保存：

- `latest_score`：最新综合分；
- `best_score`：历史最高分；
- `last_good_score`：最近一次完整且合格分数；
- 逐次原始报告和稳定性快照。

但 `baseline_score` 保持不变。当前正在使用的节点不会因为上涨而被替换；非当前节点
可以用最新有效分参与候选排序。曾经降级的节点，只有在连续两次完整检测恢复到基准
减去恢复裕量以内后，才重新进入候选池。

这样防止基准不断上调导致“分数涨过一次，之后正常波动 20 分就误切换”。基准只能通过
人工审计命令重置，并记录旧基准、新基准、操作者和原因。

### 6.3 IP 变化

出口 IP 变化时：

1. 保存旧 IP 的报告、基准和历史；
2. 将节点标记为 `ip_changed`；
3. 不用旧 IP 的分数判断新 IP；
4. 对新 IP 重新执行完整质量检测；
5. 新 IP 完整且置信度达标后建立新基准；
6. 新 IP 在建立基准前不能作为其他节点的候选依据；
7. 如果 IP 恢复为旧 IP，恢复旧 IP 身份的历史基准，而不是覆盖它。

IP 检测默认以 IPv4 为主身份，IPv6 独立记录，避免 IPv6 偶发变化造成误切换。

## 7. 粘性和实时决策联动

实时 guardian 仍然每 15 秒执行厂商入口探测。质量 daemon 只提供经过校验的推荐文件，
实时 guardian 应在使用前再次确认 provider、节点、IP、报告新鲜度和连通性。

当前锁定节点满足以下条件时保持不变：

```text
实时关键入口可连通
且 effective_score > baseline_score - 20
```

当：

```text
effective_score <= baseline_score - 20
```

且报告完整、置信度达标时，节点解除粘性，进入重新筛选。分数下降本身不允许质量 daemon
直接改生产组；只有实时 guardian 找到一个已完成质量检测、实时可达、provider `alive`
和健康历史均通过的候选时，才允许写入供应商组。没有合格候选则保持当前节点并报警。

实时连接故障继续使用现有失败阈值和主备决策，不等待月度质量扫描。主备切换和供应商
内节点切换都必须记录完整证据、旧节点、新节点、分数、稳定性表、报告时间和原因。

候选排序固定为：

1. 已验证且未被基准下降 20 分规则淘汰的当前锁定节点；
2. 有效报告新鲜、连通性通过且综合分最高的节点；
3. 分数相同按 provider 原始顺序；
4. 不使用随机选择，不因一次较低延迟替换粘性节点。

## 8. 持久化与日志

新增目录：

```text
/guardian/data/ipquality/
├── latest-main.json
├── latest-backup.json
├── nodes/node-id.json
├── history/timestamp-channel-node.json
├── stability.json
├── stability-history.jsonl
├── recommendations.json
├── scan-progress.json
└── scan.lock
```

所有 JSON 使用临时文件、`fsync` 和原子 rename。损坏文件保留 `.corrupt-UTC-timestamp`，
不能清空已有可审计数据。报告保留数量和过期策略由唯一 `guardian.yaml` 配置控制。

日志事件至少包括：

- `quality_scan_started`、`quality_node_started`、`quality_node_completed`；
- `quality_ip_changed`、`quality_report_incomplete`、`quality_source_failed`；
- `stability_snapshot`、`stability_unknown`；
- `quality_baseline_created`、`quality_score_updated`、`quality_baseline_drop`；
- `quality_recommendation_created`、`quality_recommendation_rejected`；
- `quality_scan_resumed`、`quality_scan_failed`。

日志和报告不得写入 controller secret、API key、订阅 token、代理认证信息或未脱敏的
查询参数。

## 9. 单一配置文件

在现有 `guardian.yaml` 中增加 `quality` 段，安装器只自动生成基础设施字段，运行策略
仍由同一文件控制。目标 ID、来源分组、provider、扫描范围和顺序均由用户修改：

```yaml
quality:
  enabled: true
  full_scan_interval: 720h
  retry_interval: 24h
  order: [primary, reserve-us]
  targets:
    - id: primary
      source_group: MAIN
      provider: provider-a
      scope: locked
      lock_key: main
      listener: http://127.0.0.1:17990
    - id: reserve-us
      source_group: BACKUP-USA
      provider: provider-b
      scope: all
      node_filter: "美国"
      lock_key: backup
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
    stale_after: 26h
    good_latency_ms: 500
    bad_latency_ms: 3000
  retention:
    reports: 90
    history_days: 180
```

`order` 中的每个 ID 必须在 `targets` 中唯一存在，且每个目标的 `source_group` 必须在
mihomo 中存在。`scope: locked` 必须有有效 `lock_key`；`scope: all` 扫描该目标的
全部节点。`provider` 和 `node_filter` 可选，但目标无法唯一发现节点来源时预检失败。
`listener` 地址由安装器自动选择并保持稳定，用户不应手工把它指向生产代理端口。
质量策略、阈值、数据源和保留期可热重载。mihomo 配置、listener 端口、隔离质量组和
provider 映射属于安装/重启 guardian 范围，不能只依赖运行时热重载。

## 10. 故障处理

- 质量 daemon API 失联：只记录并退出，由 launcher 重启质量 daemon；mihomo 和实时
  guardian 不受影响；
- quality listener 失效：本轮节点标记未验证，不尝试通过生产代理组代替；
- IP 查询源超时：保留已有结果，降低置信度；不将缺失源判为风险或干净；
- provider 历史过期：不建立基准，不更新推荐；
- 单节点扫描崩溃：保存失败事件，继续固定顺序扫描其余节点；
- 推荐文件损坏或过期：实时 guardian 忽略推荐，继续原有连通性决策；
- mihomo 配置注入失败：恢复 mihomo 配置备份，不停止或 kill mihomo 进行修复；
- guardian 或质量 daemon 崩溃：launcher 只重启对应子进程；
- mihomo 自身退出：仅由 launcher 按原容器生命周期处理，不能把质量故障当作 mihomo
  故障。

## 11. 验收标准

实现必须在不触碰生产 `CHANNEL` 的测试中证明：

1. 每个 `scope: all` 目标的全部节点按用户配置顺序扫描，单节点失败不阻止后续节点；
2. 质量组切换节点时生产组和 `CHANNEL` 选择保持不变；
3. 所有外部请求都到达对应 loopback listener，不能直连；
4. mihomo 原生历史能被按小时汇总，样本不足时为 unknown；
5. 首次有效报告建立基准，后续分数上涨更新 latest/best 但不改变 baseline；
6. 当前 IP 变化会创建新的 IP 身份基准，旧 IP 历史可恢复；
7. 当前分数下降不足 20 分保持节点不变；下降达到 20 分但没有合格候选时不切换；
8. 达到 20 分下降且候选完整、可达、健康时，由实时 guardian 审计后切换供应商节点；
9. 质量 daemon 崩溃、超时、API 失联时，mihomo 和实时 guardian 持续运行；
10. 容器/guardian 重启后从持久化进度、基准、稳定性表和历史继续；
11. 配置热重载失败继续使用旧配置，报告和日志不泄露 secret；
12. 安装器预检、备份、回滚和 `git diff --check`、Go 测试、静态构建全部通过。

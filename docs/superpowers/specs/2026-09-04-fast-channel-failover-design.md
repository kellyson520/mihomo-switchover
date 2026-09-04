# 快速且可交叉验证的主备渠道切换设计

## 背景

当前 guardian 主循环约每 15 秒运行，但公网探测结果按 `probe_interval`（生产为
5 分钟）缓存。`failures_before_switch: 3` 因此会把三次独立失败拉长到约 8--15 分钟。
同时，主 provider 的原生健康检查可能在备用渠道运行期间保持旧的 `alive: false`，而
备用 provider 的地区过滤器在订阅名称变化时可能匹配不到任何节点。

## 目标

- 首次确认失败后，正常情况下 60--90 秒内完成切换，最迟不超过 2 分钟。
- 不因为单个厂商暂时返回异常、HTTP 5xx、401/403/429 或一次网络抖动误切换。
- 正常状态仍保持低频公网探测，故障确认只在故障窗口短暂增加请求。
- 备用节点在故障前持续由 mihomo 原生健康历史验证并保持 sticky 选择。
- 过滤器不会把备用 provider 变成空集合；过滤策略与缓存节点不匹配时在部署前失败。
- guardian 的任何健康判断或切换失败都不能停止、重启或影响 mihomo 进程。

## 方案

### 1. 自适应探测节奏

`decision.probe_interval` 继续控制正常状态缓存（生产保持 `5m`）。新增
`decision.failure_recheck_interval`，默认 `30s`，仅当最近一次关键探测结果为不健康时
生效。每次复核都必须取得新样本，不重复累计同一个缓存结果。

生产默认仍为两个关键厂商（OpenAI、Gemini）并行探测。沿用
`failures_before_switch: 3`：首次失败、约 30 秒后的第一次复核、约 60 秒后的第二次复核
均失败，且备用节点已通过 mihomo `alive: true` 和非空 history 验证，才写入
`CHANNEL=BACKUP`。这样不会把正常状态的请求变成高频轮询，故障窗口最多增加两轮有界请求。

### 2. 失败分类与交叉验证

- `reachable_http`（包括配置允许的 2xx--499，如未授权响应）表示该厂商路径可达。
- DNS、TCP、TLS、连接或响应超时属于网络级失败，可作为节点失败证据。
- `upstream_http_error`（5xx）表示厂商自身异常，不单独归因于代理节点。
- 其他 HTTP 响应表示路径可达但契约异常，不单独触发切换。

切换仍以关键探测 quorum 为准；必须是独立厂商的网络级失败持续满足 quorum，不能由一个
厂商单独触发。mihomo provider 健康历史作为第二类证据，只有 `alive: true` 且存在历史
的备用节点才可作为切换目标。

### 3. 恢复路径

备用运行期间继续每个 guardian 周期读取主 provider 元数据；未验证时按独立的
`decision.recovery_healthcheck_interval`（生产默认 `2m`）请求 mihomo 原生
healthcheck。该请求不切换节点、不写 `CHANNEL`，并受单 provider 限频。主节点恢复后仍
需要两次连续恢复确认和最短保持时间，避免主备来回抖动。

主渠道探测缓存键在 provider 暂时未验证时继续使用当前实际节点，而不是退化成空节点键，
避免旧缓存被错误复用或每个循环重新计数。

### 4. 备用过滤器防空

当前生产备用 provider 使用地区过滤。过滤器扩展为兼容中文地区名、美国国旗和常见英文
名称；安装/更新前读取 provider 缓存并验证至少有一个代理名称匹配。验证失败时终止安装
并保留旧配置，不让 mihomo 进入空 provider。guardian 运行时对空集合继续 fail-closed，
记录明确的 provider-unverified 事件，不盲选未被 mihomo 识别的节点。

过滤器仍属于 mihomo 配置，不写死在 guardian 决策代码中；后续用户可以在唯一配置文件
中按自己的分组和命名规则修改。

## 生产安全

- 只替换 guardian 二进制/配置和必要的 provider 过滤配置。
- 不执行 `docker stop`、`docker restart`、Compose 重建或向 mihomo PID 发信号。
- guardian 的 provider healthcheck 请求失败只记录日志，不使运行循环退出。
- 生产验收必须确认 mihomo PID 不变、容器仍运行、CHANNEL 当前选择未被意外改变，且
  guardian 日志包含探测分类和切换耗时。

## 测试

- 配置默认值和热重载字段测试。
- 红测：健康结果缓存期间失败不会重复计数；失败后 30 秒间隔取得新样本并在第三次
  失败时生成切换动作。
- 红测：单个厂商 5xx 或单次网络抖动不满足交叉验证时不切换。
- 红测：主 provider 节点暂时未验证时探测键保持当前节点。
- provider 过滤缓存无匹配时安装前拒绝，兼容中文/国旗/英文名称的过滤可匹配当前备用
  缓存。
- 执行全量 Go 测试、race、vet、Python 测试和静态 Linux 构建后，才更新生产 guardian。

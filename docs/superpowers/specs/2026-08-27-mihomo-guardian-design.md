# Mihomo Guardian 设计文档

**日期：** 2026-08-27  
**状态：** 待用户审阅  
**项目位置：** `/上传/mihomo-guardian`

## 1. 目标

为后续号池提供一个注入到 mihomo 容器内部运行的线路守护程序，重点保证代理稳定、切换有证据、节点尽量固定、容器重建和重启不丢状态。

第一版必须覆盖：

1. 主渠道与备用渠道的自动切换，以及人工切换入口。
2. 针对 OpenAI、Gemini 和其他可配置厂商入口的线路探测。
3. 每个供应商独立保存上次成功节点；供应商恢复或再次切入时优先使用该节点。
4. 可选的出口 IP/ASN/组织和信誉信息检测，但纯净度结果只作为辅助评分和告警，不能凭一次结果自动切换。
5. 持久化日志、健康历史、节点锁定和切换状态。
6. 单一控制器配置文件热重载，错误配置不影响当前运行配置。

号池不是本项目的一部分，不在 mihomo 容器中运行，也不由守护程序读取或改写号池数据。

## 2. 现有环境约束

当前容器为 `mihomo-cliproxy`，镜像为 `metacubex/mihomo:Alpha`，容器重启策略为 `unless-stopped`。现有挂载包括：

- `/opt/mihomo-cliproxy/config/config.yaml` → mihomo 配置，只读；
- `/opt/mihomo-cliproxy/providers` → provider 文件，可写；
- mihomo 配置目录的 Docker volume。

当前已有宿主机 systemd 服务和 `/opt/mihomo-cliproxy/channel_switch.py`。新项目安装时必须先停止并替换旧切换器，避免两个进程同时写 `CHANNEL`；旧脚本和 unit 文件以带时间戳的备份保留，不能直接删除。

当前代理组语义为 `MAIN`、`BACKUP-USA`、`CHANNEL`、`PROXY`。其中 `CHANNEL` 选择主备，`PROXY` 选择 `CHANNEL` 或 `DIRECT`。新配置通过组名映射兼容该命名，同时允许后续号池使用其他组名。

## 3. 方案选择

采用“静态控制器通过挂载注入，控制器作为同容器监督进程”的方案：

- 不修改官方 mihomo 镜像内容；
- 将静态可执行文件、启动配置和启动器挂载到 mihomo 容器；
- 将容器启动命令改为守护程序，由守护程序启动并监督 `/mihomo`；
- 守护程序和 `/mihomo` 在同一个容器、同一个网络命名空间内；
- 所有可变数据写到宿主机挂载区；
- mihomo 镜像升级只需要重新执行一键安装/同步脚本，不需要把业务逻辑重新打进镜像。

宿主机 systemd 注入只作为迁移前的兼容方式，不作为正式运行路径，因为它会产生容器重启后的注入空窗。基于自定义 mihomo 镜像也不采用，因为会增加镜像升级和回滚成本。

## 4. 运行时架构

控制器是一个静态 Go 程序，职责分为五个边界清晰的模块：

### 4.1 Supervisor

控制器作为容器 PID 1：

- 启动 `/mihomo -d /root/.config/mihomo`；
- 将 SIGTERM/SIGINT 转发给 mihomo，并等待其退出；
- mihomo 异常退出时记录原因并退出，让 Docker `unless-stopped` 负责恢复；
- 控制器自身异常退出时同样由 Docker 拉起；
- 不在控制器内无限重启 mihomo，避免故障时快速重启打满日志或影响号池。

### 4.2 Mihomo API 客户端

通过 `http://127.0.0.1:9090` 调用 mihomo REST API，认证复用现有 `.controller_secret` 文件，不把密钥打印到日志。使用的接口包括：

- `GET /proxies`：读取渠道、节点和当前选择；
- `GET /proxies/{node}/delay`：不改变当前流量的单节点快速探测；
- `PUT /proxies/{group}`：锁定供应商组节点或切换 `CHANNEL`；
- `GET /configs`、`PUT /configs`：仅在配置明确开启时执行 mihomo 配置热加载。

所有 API 请求有独立的超时、重试上限和错误分类。控制器 API 不可达时只记录 `mihomo_api_unavailable`，不凭 API 故障切换渠道。

### 4.3 Probe Engine

探测分为两层：

1. 节点层：通过 mihomo `/delay` 对节点进行低成本并发探测，用于筛选候选节点，不改变当前选择。
2. 当前线路层：对当前锁定节点经本地代理访问厂商入口，读取 DNS、TCP、TLS、HTTP 状态和耗时，用于切换决策。

默认厂商探测入口写在控制器配置中，第一版预置：

- OpenAI：`https://api.openai.com/v1/models`；
- Gemini：`https://generativelanguage.googleapis.com/v1beta/models`；
- Anthropic：`https://api.anthropic.com/v1/models`；
- OpenRouter：`https://openrouter.ai/api/v1/models`；
- DeepSeek：`https://api.deepseek.com/models`。

这些请求不带 API Key。HTTP 200–499（含 401、403、429）表示厂商入口已到达；DNS 失败、连接失败、TLS 失败、超时和 HTTP 5xx 表示本次线路探测失败。入口、方法、期望状态、超时和是否参与切换均可在单一配置文件中修改。

### 4.4 Decision Engine

默认策略为保守切换：

- 探测周期 15 秒；
- 当前关键厂商连续 3 个周期失败才进入故障确认；
- 单个厂商故障不会立即触发切换；只有关键检查达到配置的失败 quorum，且备用候选通过验证时才切换；
- 切换后至少保持 120 秒，冷却期间不因单次波动切回；
- 主渠道连续 2 个周期恢复且验证成功后切回；
- 主备都无法通过验证时保持当前选择并告警，不盲切到未知节点；
- API 暂时不可用、配置解析失败、纯净度服务不可用均不能单独触发切换。

每次决策都会带上证据：当前组、当前节点、各厂商结果、连续失败计数、候选节点结果、决策原因和最终 API 写入结果。

### 4.5 Purity Advisor

纯净度检测默认不开启自动决策。对通过基础可用性检查的候选节点，经本地代理查询可配置的 IP 信息入口，记录：出口 IP、国家/地区、ASN、组织、是否数据中心（若服务提供该字段）、多入口 IP 是否一致。

信誉 API 使用可选的 HTTP JSON 适配器，不内置需要密钥的第三方账号。信誉结果只产生 `purity_warning` 或评分字段；只有用户显式配置为“硬门槛”并且连续多次得到相同结果时，才允许将其作为候选淘汰条件，不能单独触发主备切换。这样避免公共信誉库误报导致号池瞬间换线。

## 5. 节点稳定与切换规则

为避免供应商内节点反复变化，第一版不使用 mihomo `url-test` 自动选最快节点作为最终策略，而将主、备供应商组设为可控的 `select` 组，由控制器显式锁定节点。`CHANNEL` 仍为 `select` 组，只指向 `MAIN` 或 `BACKUP-USA`。

每个供应商维护一份持久化锁定记录：

```json
{
  "provider": "main",
  "group": "MAIN",
  "node": "节点名称",
  "last_verified_at": "2026-08-27T19:00:00+08:00",
  "failure_streak": 0
}
```

候选选择顺序固定为：

1. 该供应商上次成功节点；
2. 该 provider 当前顺序中的其他节点；
3. 通过基础探测且风险评分最高的节点；
4. 同分时保持 provider 原始顺序，不随机化。

只要锁定节点仍通过关键厂商检查，就不因为延迟更低的新节点而切换。更换节点需要连续失败证据，并且更换后写入锁定记录。切换主备时，会先恢复目标供应商自己的上次节点，再做一次目标节点验证；验证失败才扫描其他节点。

## 6. 配置与持久化布局

项目仓库只保存示例配置和程序，运行时目录建议挂载到 `/opt/mihomo-cliproxy/guardian`：

```text
/opt/mihomo-cliproxy/guardian/
├── bin/guardian                 # 静态控制器，代码只读
├── guardian.yaml                # 唯一行为配置，人工/AI 主要编辑此文件
├── data/
│   ├── state.json               # 当前主备、失败计数、冷却时间
│   ├── provider-locks.json      # 每个供应商最后成功节点
│   └── health-history.jsonl     # 精简健康历史
├── logs/
│   ├── guardian.jsonl           # 结构化运行日志
│   └── guardian.jsonl.*         # 自动轮转文件
└── run/                         # 临时 PID/锁，不承载关键状态
```

`guardian.yaml` 包含 mihomo API 地址、组映射、厂商入口、探测参数、切换阈值、纯净度策略、日志策略和热重载选项。mihomo 原有 `config.yaml`、provider 文件和 controller secret 继续由 mihomo 自己管理；控制器不复制密钥，也不要求号池修改配置。

状态文件采用临时文件写入后 `rename` 替换，启动时校验 JSON；损坏时保留原文件为 `.corrupt.<timestamp>` 并使用安全默认值。日志使用 JSONL、按大小和天数轮转，所有 URL 查询参数中的 token、secret、key、密码字段统一脱敏。

## 7. 热重载与人工控制

控制器每 2 秒检查 `guardian.yaml` 的修改时间。发现变化后先读入临时副本、解析并校验完整配置；成功才原子替换运行配置，失败则继续使用旧配置并写入 `config_reload_failed`。

支持通过本地命令执行以下人工操作：

- 查看状态、当前渠道、当前节点和最近探测结果；
- 强制切到主渠道或备用渠道；
- 解除强制模式，恢复自动决策；
- 立即刷新 provider；
- 立即执行一次完整探测；
- 重新加载控制器配置。

人工强制模式默认有时间上限，过期后自动恢复；强制操作不会删除节点锁定记录。

## 8. 故障与安全边界

- 控制器不能访问 mihomo API：保持当前流量路径，不写 `CHANNEL`。
- 当前节点失败但备用节点未验证：保持当前路径并告警，不盲切。
- 厂商整体 5xx 或公共网络故障：通过多节点交叉结果识别为上游故障，不把所有节点判死。
- provider 更新中：继续使用旧 provider 文件，更新完成并可解析后才切换到新节点集合。
- provider 节点名称消失：记录锁定失效，按原始 provider 顺序选择首个通过验证的节点。
- 配置热重载失败：保留上一份有效配置。
- 进程退出：由 Docker 重启容器；状态和日志在容器外挂载区。
- 控制 API 只绑定容器内 `127.0.0.1` 访问路径；若现有 mihomo 对外监听 9090，安装器不扩大暴露面，并确保 secret 不出现在进程参数和日志中。

安装器在改动 compose、mihomo 配置和 systemd 前逐份创建带时间戳备份，并提供可执行回滚脚本。首次部署执行健康检查、API 读写检查和“无切换演练”，确认通过后才启用自动切换。

## 9. 测试与验收标准

单元测试覆盖：

- 200–499 与 5xx/超时的状态分类；
- 连续失败阈值、恢复阈值、冷却期和 quorum 决策；
- 主备切换和人工强制优先级；
- provider 锁定节点优先级和节点消失后的确定性选择；
- 状态原子保存、损坏恢复和重启恢复；
- 配置热重载成功、语法错误回退和未知字段校验；
- 日志脱敏与轮转。

集成测试使用本地 HTTP/TLS 测试服务和 mihomo API 测试替身，不向真实厂商发送带密钥请求。安装验收包括：

1. 执行一键安装后容器保持原端口和网络连通性；
2. `docker restart mihomo-cliproxy` 后控制器和 mihomo 都自动恢复，当前渠道与锁定节点从挂载状态恢复；
3. 模拟主渠道连续失败时只切换一次，模拟恢复时按阈值切回；
4. 模拟 provider 更新、控制 API 暂时不可达和配置错误时号池流量不被误切；
5. 日志能完整解释每一次探测与切换原因，且不出现 secret、token 或 API Key。

## 10. 迁移和回滚

安装流程顺序：


1. 检查 Docker、现有容器、配置、挂载和旧 systemd 服务；
2. 在 `/opt/mihomo-cliproxy/guardian` 创建数据目录并写入默认配置；
3. 备份现有 compose、mihomo 配置和旧切换器；
4. 停止旧 systemd 切换器，确保只有一个控制器写 API；
5. 将 `MAIN`、`BACKUP-USA` 调整为稳定选择组，保留原 provider 和 `CHANNEL/PROXY` 关系；
6. 注入静态控制器挂载和容器启动命令，平滑重建 mihomo 容器；
7. 做只读状态检查和无切换探测；
8. 验证通过后启用自动决策。

回滚流程从最近一次备份恢复 compose 和 mihomo 配置，停止 guardian 容器内控制器，恢复旧 systemd 服务；provider、状态和日志目录不删除，便于审计和再次安装。

## 11. 非目标

- 不承诺任何公共 IP 信誉服务能准确证明“纯净”；
- 不使用 API Key 模拟真实账号调用；
- 不管理号池、账号、额度、登录态或业务请求；
- 不随机轮换节点，不以最低延迟为理由频繁更换出口；
- 不在无法取得足够证据时强行切换。

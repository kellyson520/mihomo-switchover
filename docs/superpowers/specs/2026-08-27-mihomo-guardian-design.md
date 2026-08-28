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

安装脚本支持直接在 mihomo 项目目录执行，不要求手工填写容器名、IP、控制端口或代理端口。脚本会从当前目录、compose 标签、容器挂载和 mihomo 配置交叉确认目标；多个候选无法唯一确认时只读失败并报告，不猜测修改线上。

## 3. 方案选择

采用“静态控制器通过挂载注入，控制器作为同容器监督进程”的方案：

- 不修改官方 mihomo 镜像内容；
- 将静态可执行文件、启动配置和启动器挂载到 mihomo 容器；
- 将容器启动命令改为轻量启动脚本；启动脚本启动 `/mihomo`，并独立拉起/重启 guardian；
- 守护程序和 `/mihomo` 在同一个容器、同一个网络命名空间内；
- guardian 依赖 mihomo 提供本地 API 和代理出口，但 guardian 崩溃、升级、配置错误或 API 失联时不得终止 mihomo；
- 所有可变数据写到宿主机挂载区；
- mihomo 镜像升级只需要重新执行一键安装/同步脚本，不需要把业务逻辑重新打进镜像。

宿主机 systemd 注入只作为迁移前的兼容方式，不作为正式运行路径，因为它会产生容器重启后的注入空窗。基于自定义 mihomo 镜像也不采用，因为会增加镜像升级和回滚成本。

## 4. 运行时架构

控制器是一个静态 Go 程序，职责分为五个边界清晰的模块：

### 4.1 Container launcher

挂载的 `start-guardian.sh` 作为容器 PID 1，mihomo 是生产代理主进程，guardian 是独立后台子进程：

- 启动 `/mihomo -d /root/.config/mihomo` 并等待它退出；
- 在 mihomo 存活期间循环拉起 guardian；guardian 非零退出或崩溃时按固定短退避重启 guardian；
- guardian 退出、卡死或重启不会发送任何信号给 mihomo；
- 只有 mihomo 自己退出时，启动脚本才结束，让 Docker `unless-stopped` 恢复整个容器；
- 收到 Docker 停止信号时，启动脚本才同时清理两个子进程。

### 4.2 Mihomo API 客户端

通过 `http://127.0.0.1:9090` 直连调用 mihomo REST API，认证复用现有 `.controller_secret` 文件，不把密钥打印到日志。控制 API 必须直连容器内回环地址，不能再经过 mihomo 代理，否则会形成循环依赖。使用的接口包括：

- `GET /proxies`：读取渠道、节点和当前选择；
- `GET /providers/proxies/{provider}`：读取 mihomo 已完成的逐节点健康状态和历史；
- `GET /proxies/{name}/delay`：对当前可直接访问的代理或代理组执行快速探测；
- `PUT /proxies/{group}`：锁定供应商组节点或切换 `CHANNEL`；
- `GET /configs`、`PUT /configs`：仅在配置明确开启时执行 mihomo 配置热加载。

普通 API 请求有独立的超时、重试上限和错误分类；普通请求失败只记录 `mihomo_api_request_failed`，不凭请求故障切换渠道。API 心跳和生命周期绑定按下述强绑定规则处理。

启动阶段必须在 `startup_api_timeout` 内完成 API 认证和基础状态读取；超时则 guardian 自身退出，由启动脚本重启 guardian，不能终止 mihomo。运行阶段每个探测周期都必须先完成 API 心跳；连续失联达到 `link_loss_grace`（默认 15 秒）后，guardian 自身退出，由启动脚本重新拉起。失联期间不执行节点扫描、主备切换或 provider 刷新。

### 4.3 Probe Engine

探测分为两层，所有访问外部厂商或纯净度服务的 HTTP 请求都必须使用 mihomo 的 `http://127.0.0.1:7890` 代理，控制器禁止直连公网：

1. 节点层：对 provider-backed 组读取直连 mihomo 控制 API 的 provider 元数据，使用
   `alive` 和健康历史筛选候选；部分 mihomo Alpha 版本不会把 provider 节点作为
   `/proxies/<节点>` 资源暴露，因此不临时把每个候选切入生产流量做 `/delay`。
   没有健康历史的候选保持未知。没有 provider 映射的静态组才使用其 API 支持的
   `/delay` 路径。
2. 当前线路层：对当前锁定节点经 mihomo 本地代理访问厂商入口，读取 DNS、TCP、TLS、HTTP 状态和耗时，用于切换决策。

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

纯净度检测默认不开启自动决策。对通过基础可用性检查的候选节点，经 mihomo 本地代理查询可配置的 IP 信息入口，记录：出口 IP、国家/地区、ASN、组织、是否数据中心（若服务提供该字段）、多入口 IP 是否一致。

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

只要锁定节点仍通过关键厂商检查，就不因为延迟更低的新节点而切换。更换节点需要连续失败证据，并且更换后写入锁定记录。切换主备时，会先恢复目标供应商自己的上次节点，再读取 mihomo
`/providers/proxies/<provider>` 中该节点的 `alive` 和健康历史；验证失败才扫描其他节点。
不临时把每个候选节点切入生产流量做 `/delay`，因为部分 mihomo Alpha 版本不会把
provider 节点作为 `/proxies/<节点>` API 资源暴露。

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

如果 `guardian.yaml` 不存在，安装器根据自动发现结果生成它；生成后所有运行策略仍集中在这一个文件。发现到的控制端口和代理端口写成容器内回环地址，宿主机发布端口只用于安装器诊断，不作为运行依赖。

状态文件采用临时文件写入后 `rename` 替换，启动时校验 JSON；损坏时保留原文件为 `.corrupt.<timestamp>` 并使用安全默认值。日志使用 JSONL、按大小和天数轮转，所有 URL 查询参数中的 token、secret、key、密码字段统一脱敏。

### 6.1 自动发现

安装器按以下顺序发现目标：

1. 当前目录是否同时包含 compose、mihomo 配置和 provider 目录；
2. 当前 compose 的 project/service 标签是否对应运行中的 mihomo 容器；
3. Docker 容器挂载源路径是否能反向定位 compose 工作目录；
4. 配置中的 `mixed-port`、`http-port`、`socks-port`、`external-controller` 和 `secret`；
5. 配置代理组中含有 `MAIN`/`BACKUP` 关系的唯一 `CHANNEL` 组，或现有默认命名；
6. provider `use` 关系和现有组节点，生成稳定的 provider 映射。

容器内 API 始终使用发现出的 `127.0.0.1:<external-controller-port>`，外部探测始终使用发现出的 `http://127.0.0.1:<mixed-port>`；如果只有 SOCKS 端口，则使用 `socks5://127.0.0.1:<socks-port>`。发现器同时校验端口为正整数、配置文件可读、组名确实存在。IP 只用于展示和 compose 诊断，不写死到 guardian 运行逻辑。

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

- guardian 启动时不能访问 mihomo API：guardian 自身退出，由启动脚本重启，mihomo 和现有代理继续运行。
- guardian 运行中不能访问 mihomo API：在 15 秒宽限期内只重试和记录；超过宽限期 guardian 自身退出，启动脚本重启 guardian，绝不终止 mihomo。
- 任何外部探测请求若绕过 mihomo 代理：视为配置/实现错误，探测结果无效，不得据此切换。
- 当前节点失败但备用节点未验证：保持当前路径并告警，不盲切。provider 健康元数据
  不可用或没有健康历史时同样 fail-closed。
- 厂商整体 5xx 或公共网络故障：通过多节点交叉结果识别为上游故障，不把所有节点判死。
- provider 更新中：继续使用旧 provider 文件，更新完成并可解析后才切换到新节点集合。
- provider 节点名称消失：记录锁定失效，按原始 provider 顺序选择首个通过验证的节点。
- 配置热重载失败：保留上一份有效配置。
- guardian 进程退出：由容器内启动脚本重启；只有 mihomo 进程退出才由 Docker 重启容器；状态和日志在容器外挂载区。
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
3. 模拟 mihomo API 启动超时或运行中失联时，guardian 不会继续决策，只退出并被启动脚本重启，mihomo 进程保持运行；
4. 模拟主渠道连续失败时只切换一次，模拟恢复时按阈值切回；
5. 模拟 provider 更新、配置错误时号池流量不被误切；
6. 模拟 guardian 崩溃、配置错误和反复重启时，mihomo 进程及 7890 代理持续可用；
7. 日志能完整解释每一次探测与切换原因，且不出现 secret、token 或 API Key。

## 10. 迁移和回滚

安装流程顺序：


1. 从当前 mihomo 目录/容器自动发现 Docker、容器、配置、挂载、端口、组映射和旧 systemd 服务；发现歧义时停止，不改线上；
2. 在发现出的 mihomo 挂载目录下创建 `guardian` 数据目录并写入生成配置；
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

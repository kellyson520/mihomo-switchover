# mihomo-guardian

这是一个注入现有 `mihomo-cliproxy` 容器的独立线路守护程序。号池不在
mihomo 容器内，guardian 不读取或修改账号、额度、登录态或业务数据。

## 安全边界

- `/mihomo` 是生产代理主进程；`start-guardian.sh` 只在其旁边启动 guardian。
- guardian 崩溃、配置错误、API 暂时失联或升级时，只重启 guardian，绝不停止或给
  mihomo 发信号。只有 Docker 正常停止容器时，启动器才同时清理两个子进程。
- 控制 API 只用于容器内回环直连；所有 OpenAI、Gemini、Anthropic、OpenRouter、
  DeepSeek 和纯净度探测都强制经自动发现的容器内回环代理端口。
- 不使用 API Key；厂商入口收到 `200–499` 通常代表入口可达，但命中显式配置的地区/线路
  政策拒绝响应体时会标记为不可用，`5xx`、DNS、TCP、TLS 和超时仍分别记录。
- 纯净度/IP/ASN 只产生告警和评分，默认不能触发自动切换。
- 配置、节点锁定、状态、日志和备份全部在宿主机挂载区，重建容器不会丢失。

## 一键安装与自动发现

在本仓库执行：

```sh
sudo ./scripts/install.sh --preflight
sudo ./scripts/install.sh
```

安装器会从当前目录、Compose 标签、容器挂载和 mihomo 配置交叉发现唯一目标，
自动得到容器名、Compose 文件、内部 `mixed-port/http-port/socks-port`、
`external-controller`、控制器 secret、代理组关系、provider 映射和宿主挂载路径。
容器内运行配置使用 `127.0.0.1:<内部端口>`，不会误把宿主发布的 `7891` 当成容器
内代理端口。候选不唯一、配置不可读或组关系不明确时停止并要求显式指定，禁止猜测。

首次安装先以 observe 模式验证 API、进程和代理入口；烟雾测试通过后才切换为 auto。
需要只观察时执行 `sudo ./scripts/install.sh --observe`。

安装器会在目标项目的 `guardian/backups/<UTC 时间>/` 保存 Compose、mihomo 配置、
guardian 二进制、launcher、controller secret、旧 systemd 切换器和清单。旧
`channel_switch.py` 不删除，但旧 systemd 服务会被停用，避免两个程序同时写 `CHANNEL`。

## 重新注入与安全更新

旧版本可能把 guardian 二进制作为单文件挂载；这会固定旧 inode，宿主机替换文件不会让
运行中的容器看到新版本。先只读确认：

```sh
sudo ./scripts/install.sh --preflight
sudo ./scripts/update-guardian.sh --preflight
```

如果输出 `migration_required=1`，必须在维护窗口执行一次：

```sh
sudo ./scripts/install.sh --migrate-bin-mount --observe
```

这次迁移把挂载改为 `/opt/mihomo-cliproxy/guardian/bin:/guardian/bin:ro`，可能重建
Compose 服务并短暂影响 Mihomo；迁移前安装器会备份并校验，迁移失败会走回滚。观察确认
Mihomo、代理端口和 guardian 正常后，再按既定命令切回 `auto`。

迁移完成后，日常更新只使用：

```sh
sudo ./scripts/update-guardian.sh --preflight
sudo ./scripts/update-guardian.sh --observe
```

更新器在持久化挂载目录内完成新 ELF/hash 校验、旧文件备份和原子 rename，只 TERM
guardian/quality 子进程，等待 launcher 拉起新版本；不会重建或重启 Mihomo，不修改
Mihomo 配置、provider、代理组、状态或质量 store。验证失败会自动原子恢复备份二进制，
记录 `update_rolled_back`，仍不触碰 Mihomo。更新过程写入
`guardian/logs/guardian-update.jsonl`，不写入 secret、API key、订阅 URL 或账号信息。

更新前后会用容器内 ps 校验 guardian/quality 的父子关系，只向 guardian 子进程发 TERM；
同时调用 guardian status 验证 Mihomo API，并检查 `/proc/net/tcp` 的代理监听。更新使用
`guardian/run/guardian-update.lock` 防止并发替换；如果出现 `update_rollback_failed`，
立即停止继续操作并人工核对备份 hash。

## 运行目录与配置

```text
/opt/mihomo-cliproxy/guardian/
├── start-guardian.sh
├── bin/guardian
├── guardian.yaml          # 唯一需要人工/AI 编辑的行为配置
├── controller_secret
├── data/state.json
├── data/ipquality/       # reports, baselines, scan progress and audit
├── logs/guardian.jsonl
├── logs/quality.jsonl
└── backups/
```

只编辑 `guardian.yaml`。guardian 每个周期在 API 心跳成功后检查配置修改时间，
只有完整解析和校验成功才应用；错误配置继续使用上一份有效配置。主渠道连续失败
达到阈值且备用节点经过 mihomo provider 健康结果验证才切换；每个供应商的上次成功
节点落盘并优先复用，不因一次低延迟结果随机换节点。对 provider 节点不做临时切入
生产流量的 `/proxies/<节点>/delay` 调用，因为当前 mihomo Alpha 对这类路径返回 404；
没有 `alive` 和健康历史的候选保持未知并拒绝切换。未验证 provider 时，guardian 会
按 `probe_interval` 请求 mihomo 原生 `/providers/proxies/<provider>/healthcheck`，
让备用运行期间的主渠道健康证据能够恢复；该请求不切换 `CHANNEL`。

主循环默认每 15 秒运行，但 OpenAI、Gemini 等公网厂商在正常状态下默认每 5 分钟刷新
一次；Mihomo 的本地 error 日志流遇到网络拨号、TLS、超时或连接重置时，会立即打破这次
缓存并进入有界快速复核。故障窗口内按 `failure_recheck_interval`（默认 30 秒）取得
新样本，缓存期间不会重复访问厂商，也不会重复累计同一个失败。修改
`decision.probe_interval` 可热重载调整正常频率，修改 `decision.failure_recheck_interval`
可调整故障确认周期；日志提示本身永远不会直接切换渠道。

Gemini 的 `400 User location is not supported` 不是普通认证失败。为避免把它算作“入口可达”，
探测配置可为 Gemini 增加 `reject_body_patterns`。命中后 guardian 会在同一 provider 的
其他 `alive + history` 节点中逐个复核，并要求所有启用的 `critical` 厂商探测通过；找到同时
满足 OpenAI 与 Gemini 的节点后才固定它。候选检查只写 provider 组，不直接改总选择器；没有
合格候选时恢复原节点并保持 fail-closed。

## 运维命令

```sh
sudo ./scripts/status.sh --read-only
docker exec mihomo-cliproxy /guardian/bin/guardian status --config /guardian/guardian.yaml
docker exec mihomo-cliproxy /guardian/bin/guardian probe --config /guardian/guardian.yaml
docker exec mihomo-cliproxy /guardian/bin/guardian switch backup --config /guardian/guardian.yaml
docker exec mihomo-cliproxy /guardian/bin/guardian auto --config /guardian/guardian.yaml
docker exec mihomo-cliproxy /guardian/bin/guardian reload --config /guardian/guardian.yaml
docker exec mihomo-cliproxy /guardian/bin/guardian quality status --config /guardian/guardian.yaml --data /guardian/data
docker exec mihomo-cliproxy /guardian/bin/guardian quality run --config /guardian/guardian.yaml --data /guardian/data --logs /guardian/logs --secret-file /guardian/controller_secret
```

`status.sh --read-only` 不创建文件、不改 API、不重启容器。人工 `switch` 有 30 分钟
保护期并写入审计日志；`auto` 清除强制状态。guardian API 失联超过宽限期时自身退出，
启动器只重启 guardian，mihomo 和代理端口继续由原进程提供服务。

`quality status` 只读显示质量 daemon、目标 listener、扫描进度、节点记录、baseline
数量和最新质量/稳定性/综合分，不回显 secret、节点订阅地址或账号信息。需要对单个目标
立即扫描时给 `quality run` 增加 `--target TARGET_ID`。只有在确认当前 IP、供应商数据和
报告确实发生长期变化时，才使用精确身份重置 baseline：

质量 daemon 每个 `stability.summary_interval` 周期只读取 mihomo 已有的 provider history，
不发起公网请求、不调用 delay、不选择节点；只有已经完成 IP 身份确认的节点才会更新
`stability.json`、`stability-history.jsonl` 和推荐输入。`quality status` 中的
`latest_stability_at` 是最近一次这类汇总时间，和全量质量报告时间分开显示。

```sh
docker exec mihomo-cliproxy /guardian/bin/guardian quality baseline-reset \
  --config /guardian/guardian.yaml --data /guardian/data \
  --target TARGET_ID --node NODE_NAME --ip EXIT_IP
```

## 给 Agent 和运维人员的文档

- [配置文件、热重载和回滚说明](docs/configuration.md)
- [生产投入 Agent Skill](skills/mihomo-guardian-production/SKILL.md)
- [仓库 Agent 工作约定](AGENTS.md)

## 回滚

```sh
sudo ./scripts/rollback.sh --guardian-root /opt/mihomo-cliproxy/guardian
```

自动失败回滚会固定使用本次安装创建的备份目录；人工回滚建议先从
`/opt/mihomo-cliproxy/guardian/backups/` 选择并显式指定目录：

```sh
sudo ./scripts/rollback.sh \
  --guardian-root /opt/mihomo-cliproxy/guardian \
  --backup-dir /opt/mihomo-cliproxy/guardian/backups/UTC-TIMESTAMP-PID
```

脚本会先严格校验 manifest 和所有备份文件，再分阶段原子恢复；中途校验、文件或
Compose 失败会尝试恢复回滚前的 host 文件。回滚恢复 Compose、mihomo 配置和旧 systemd 服务；guardian 的
状态、质量 reports/baselines/scan progress、日志和备份目录保留，不删除节点或审计数据。

## 本地验证

宿主机没有 Go 时，使用 mihomo 的网络命名空间作为构建环境；Go 依赖已 vendor，
构建步骤不访问公网，也不会把代理端口写死：

```sh
make check CONTAINER=mihomo-cliproxy
make build CONTAINER=mihomo-cliproxy
pytest -q
```

`dist/guardian` 是静态 Linux amd64 二进制，正式复制前应检查 `file dist/guardian`。

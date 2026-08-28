# mihomo-guardian

这是一个注入现有 `mihomo-cliproxy` 容器的独立线路守护程序。号池不在
mihomo 容器内，guardian 不读取或修改账号、额度、登录态或业务数据。

## 安全边界

- `/mihomo` 是生产代理主进程；`start-guardian.sh` 只在其旁边启动 guardian。
- guardian 崩溃、配置错误、API 暂时失联或升级时，只重启 guardian，绝不停止或给
  mihomo 发信号。只有 Docker 正常停止容器时，启动器才同时清理两个子进程。
- `9090` 只用于容器内回环控制 API，必须直连；所有 OpenAI、Gemini、Anthropic、
  OpenRouter、DeepSeek 和纯净度探测都强制经容器内 `7890` 代理。
- 不使用 API Key；厂商入口收到 `200–499` 都代表入口可达，`5xx`、DNS、TCP、TLS
  和超时才算本次探测失败。
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
旧 systemd 切换器和清单。旧 `channel_switch.py` 不删除，但旧 systemd 服务会被停用，
避免两个程序同时写 `CHANNEL`。

## 运行目录与配置

```text
/opt/mihomo-cliproxy/guardian/
├── start-guardian.sh
├── bin/guardian
├── guardian.yaml          # 唯一需要人工/AI 编辑的行为配置
├── controller_secret
├── data/state.json
├── logs/guardian.jsonl
└── backups/
```

只编辑 `guardian.yaml`。guardian 每个周期在 API 心跳成功后检查配置修改时间，
只有完整解析和校验成功才应用；错误配置继续使用上一份有效配置。主渠道连续失败
达到阈值且备用节点经过 mihomo provider 健康结果验证才切换；每个供应商的上次成功
节点落盘并优先复用，不因一次低延迟结果随机换节点。对 provider 节点不做临时切入
生产流量的 `/proxies/<节点>/delay` 调用，因为当前 mihomo Alpha 对这类路径返回 404；
没有 `alive` 和健康历史的候选保持未知并拒绝切换。

## 运维命令

```sh
sudo ./scripts/status.sh --read-only
docker exec mihomo-cliproxy /guardian/bin/guardian status --config /guardian/guardian.yaml
docker exec mihomo-cliproxy /guardian/bin/guardian probe --config /guardian/guardian.yaml
docker exec mihomo-cliproxy /guardian/bin/guardian switch backup --config /guardian/guardian.yaml
docker exec mihomo-cliproxy /guardian/bin/guardian auto --config /guardian/guardian.yaml
docker exec mihomo-cliproxy /guardian/bin/guardian reload --config /guardian/guardian.yaml
```

`status.sh --read-only` 不创建文件、不改 API、不重启容器。人工 `switch` 有 30 分钟
保护期并写入审计日志；`auto` 清除强制状态。guardian API 失联超过宽限期时自身退出，
启动器只重启 guardian，mihomo 和代理端口继续由原进程提供服务。

## 给 Agent 和运维人员的文档

- [配置文件、热重载和回滚说明](docs/configuration.md)
- [生产投入 Agent Skill](skills/mihomo-guardian-production/SKILL.md)
- [仓库 Agent 工作约定](AGENTS.md)

## 回滚

```sh
sudo ./scripts/rollback.sh --guardian-root /opt/mihomo-cliproxy/guardian
```

回滚前检查最新备份清单，恢复 Compose、mihomo 配置和旧 systemd 服务；guardian 的
状态、日志和备份目录保留，不删除节点或审计数据。

## 本地验证

宿主机没有 Go 时，使用 mihomo 的网络命名空间作为构建器出口：

```sh
make check CONTAINER=mihomo-cliproxy
make build CONTAINER=mihomo-cliproxy
pytest -q
```

`dist/guardian` 是静态 Linux amd64 二进制，正式复制前应检查 `file dist/guardian`。

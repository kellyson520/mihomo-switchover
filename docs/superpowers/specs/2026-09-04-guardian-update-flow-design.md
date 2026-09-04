# Guardian 注入与更新流程设计

## 目标

让 Gemini/OpenAI 探测修复可以安全进入已经注入的 `mihomo-cliproxy`，并吸取
单文件 bind mount 更新失败的经验，建立可重复、可回滚、不会扩大到 Mihomo 的更新流程。

## 约束

- Mihomo 是生产进程；任何 guardian 更新失败、崩溃、配置错误或 API 失联都不得停止
  Mihomo，也不得向 Mihomo PID 发送信号。
- 号池在容器外，本流程不读取账号、token、额度、登录态或业务数据。
- 所有公网行为仍必须经 Mihomo loopback 代理；构建使用 vendor，不因更新绕过代理访问公网。
- 更新脚本只能向 guardian 子进程发送 TERM；容器重建是唯一会影响 Mihomo 的步骤，必须显式
  指定迁移参数并在维护窗口执行。

## 根因

旧 Compose 注入把宿主文件
`/opt/mihomo-cliproxy/guardian/bin/guardian` 挂载到容器内同名文件。Docker 对该挂载保持
打开的旧 inode；宿主机替换源文件或容器内 `mv` 都不会改变当前容器看到的文件。因此新二进制
虽然写入宿主机，运行中的 guardian 仍继续执行旧版本。

## 设计

### 1. 首次迁移

`compose_patch.py` 将 guardian 二进制改为目录挂载：

```text
/opt/mihomo-cliproxy/guardian/bin:/guardian/bin:ro
```

`install.sh --preflight` 只读识别当前挂载类型，并在旧单文件挂载时报告
`migration_required=1`。非预检安装遇到旧挂载必须显式使用 `--migrate-bin-mount`；否则在任何
生产写入前退出。迁移路径先完成备份、Compose 校验、mihomo 配置校验和新二进制构建，再用
`docker compose up -d --force-recreate <service>` 完成一次受控迁移。失败时恢复部署文件并走
已有回滚路径。文档明确这一步需要维护窗口，因为 Compose 重建可能短暂重启 Mihomo。

### 2. 日常更新

新增 `scripts/update-guardian.sh`，只接受已经使用目录挂载的容器。它会：

1. 只读确认容器运行、guardian 目录挂载、guardian 配置/secret/launcher 存在且路径明确；
2. 用当前仓库和容器网络命名空间构建 Linux amd64 二进制，不读取或打印敏感配置；
3. 检查新文件是可执行 ELF、计算 SHA-256，并把旧二进制保存到 guardian 备份目录；
4. 在同一 guardian `bin` 目录创建临时文件，完成 fsync/权限/哈希校验后原子 rename；
5. 只查找并 TERM guardian/quality 子进程，等待 launcher 拉起新版本；不调用
   `docker stop`、`docker restart`、`docker kill`、`docker compose down`，不向 Mihomo 发信号；
6. 验证 Mihomo PID、运行状态、代理端口和 guardian 新版本；任一 guardian 验证失败时恢复旧
   二进制并只再次 TERM guardian，仍不触碰 Mihomo。

`--preflight` 只执行第 1 步及新二进制目标检查，不构建、不写文件、不发送信号。
更新脚本支持 `--observe`，用于只更新并观察 guardian，而不改变 guardian 的自动决策配置。

### 3. 运行时安全

目录挂载允许容器通过路径查到新 inode，但 launcher 仍持有旧 guardian 子进程；因此原子替换
之后必须只重启 guardian 子进程。launcher 的 guardian loop 会保持 Mihomo 子进程不动并拉起新
guardian，quality daemon 同理。更新脚本记录 `update_started`、`binary_backed_up`、
`guardian_reloaded`、`update_verified` 或 `update_rolled_back`，日志不包含 secret、订阅 URL、
API key 或完整 provider JSON。

### 4. 幂等与失败关闭

- Compose patch 多次执行输出完全相同，旧单文件目标只能被目录目标替换，不能同时存在。
- 目录挂载的日常更新不修改 Compose、mihomo 配置、provider、代理组或状态 store。
- 发现多个容器、多个 guardian 挂载、路径为 symlink、挂载模式不是只读或运行状态不明确时拒绝
  更新；不猜测目标。
- 新文件校验失败、launcher 未拉起 guardian、Mihomo PID 改变或代理端口失效时自动回滚二进制，
  并报告需要人工维护，不执行容器重建。

## 验证

- Python 单元测试覆盖目录挂载、旧单文件迁移、幂等、禁止双挂载及保持 Mihomo 字段不变。
- Shell 合同测试覆盖预检只读、目录挂载门禁、原子替换、备份、只 TERM guardian、失败恢复和
  禁止危险 Docker 命令。
- 提交前运行 `git diff --check`、`pytest -q`、`make check`、`make build`、`file dist/guardian`
  和生产只读状态检查。若当前生产仍是单文件挂载，只执行预检，不擅自重建容器。

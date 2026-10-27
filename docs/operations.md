# 运维与部署

## 环境要求

- Go 1.24+（源码构建；`os.Root` 需要 1.24）
- 地端：Windows 10+ / macOS / Linux，amd64 或 arm64
- 云端网关：Linux/Windows/macOS 均可；参考网关只依赖 Go 标准库和 `coder/websocket`
- 生产环境需要 TLS 反向代理（Caddy/Nginx）提供 `wss://`

## 构建

```bash
make build          # bin/termio, bin/termio-gateway；Windows 额外产出 bin/termio-agent.exe
make build-all      # dist/ 下 6 个平台 + Windows GUI agent
make test
make vet
```

Windows 没有 make 时：

```powershell
powershell -ExecutionPolicy Bypass -File scripts/build-all.ps1 -Version 0.1.0
```

交叉编译产物在 Windows 上不带可执行位；复制到 Linux/macOS 后执行 `chmod +x`。

版本注入：

```bash
go build -trimpath -ldflags "-s -w -X termio/internal/version.Version=0.1.0" -o bin/termio ./cmd/termio
```

## 配置

默认路径：

| 系统 | 配置 |
|---|---|
| Windows | `%USERPROFILE%\.lares\.termio\config.json` |
| macOS | `~/.lares/.termio/config.json` |
| Linux | `~/.lares/.termio/config.json` |

也就是说：

```
$LARES          = 端子 app home，默认 ~/.lares（Windows %USERPROFILE%\.lares）
$LARES/.termio  = 端子信息：config.json、termio.log 等
默认工作区       = $LARES
```

默认日志：`$LARES/.termio/termio.log`（Windows `%USERPROFILE%\.lares\.termio\termio.log`，macOS/Linux `~/.lares/.termio/termio.log`）。

`$LARES` 可以用环境变量 `TERMIO_HOME` 或 `LARES_HOME` 覆盖；例如：

```bash
# Windows PowerShell
$env:LARES_HOME = "C:\Users\Administrator\.laes"
# Linux/macOS
export TERMIO_HOME="$HOME/.lares"
```

默认工作区不存在时由 `termio init` 创建。端子信息目录 `.termio` 永远不对工具开放（即使默认工作区就是 `$LARES`，工具也无法读取其中的 config/token/log）。

> **从旧版本升级**：如果新路径不存在、但旧的 `UserConfigDir/termio/config.json`（Windows `%AppData%\termio\config.json`）存在，首次 `run` / `install` / `workspace` 会自动迁移到 `$LARES/.termio/config.json`；旧配置的第一个 `allowed_roots` 变成 workspace，旧的默认日志路径会改成新默认日志。旧文件保留不删。

完整示例：

```json
{
  "gateway_url": "wss://gateway.example.com/ws",
  "device_id": "dev_0e59845886b548db6f5dcaf4df7d818a",
  "device_name": "my-laptop",
  "token": "device-token",
  "workspace": "C:\\Users\\alice\\.lares",
  "allowed_roots": [],
  "max_concurrent_calls": 4,
  "call_timeout_ms": 30000,
  "log_level": "info",
  "log_file": "C:\\Users\\alice\\.lares\\.termio\\termio.log"
}
```

| 字段 | 必填 | 说明 |
|---|---|---|
| `gateway_url` | 是 | `wss://`；仅回环允许 `ws://` |
| `device_id` | 是 | 1–128 个字母/数字/`.`/`-`/`_`；`init` 自动生成 |
| `device_name` | 否 | 展示名，默认主机名 |
| `token` | 是 | 设备或共享 Bearer token |
| `workspace` | 是 | 默认工作目录；默认 `~/.lares`；必须存在且是目录 |
| `allowed_roots` | 否 | **额外**允许目录；绝对路径；不要放 workspace 的副本 |
| `max_concurrent_calls` | 否 | 1–32，默认 4 |
| `call_timeout_ms` | 否 | 1000–300000，默认 30000 |
| `log_level` | 否 | `debug` / `info` / `warn` / `error` |
| `log_file` | 否 | 绝对路径；默认 `$LARES/.termio/termio.log` |

> 工作区语义、相对路径和迁移规则见 [workspace.md](workspace.md)。`workspace` 由本地用户选择，云端不能更改。

配置按严格模式解析，未知字段会报错。Unix 文件权限 0600。

## CLI

```bash
termio init --token <token> [--workspace <dir>] [--root <extra-dir>]... [--url <ws-url>] [--log-file <path>] [--log-level <level>] [--config <path>]
termio run [--config <path>]
termio workspace show [--config <path>]
termio workspace set [--config <path>] <directory>
termio workspace reset [--config <path>]
termio install [--config <path>]
termio uninstall
termio start
termio stop
termio status
termio version
```

### 初始化示例

Linux/macOS：

```bash
# 选择已有目录作为工作区
termio init \
  --url wss://gateway.example.com/ws \
  --token "$TERMIO_TOKEN" \
  --workspace "$HOME/projects/acme" \
  --log-file "$HOME/.lares/.termio/termio.log"

# 默认工作区 ~/.lares；额外根用 --root（可重复）
termio init --token "$TERMIO_TOKEN"
termio init --token "$TERMIO_TOKEN" --workspace "$HOME/.lares" --root "$HOME/shared"
```

Windows PowerShell：

```powershell
.\termio.exe init `
  --url wss://gateway.example.com/ws `
  --token $env:TERMIO_TOKEN `
  --workspace "$env:USERPROFILE\projects\acme" `
  --log-file "$env:USERPROFILE\.lares\.termio\termio.log"
```

- `--workspace` 省略时使用默认 `~/.lares`，不存在会自动创建。
- `--workspace` 显式指定时目录必须已存在。
- `--root` 是额外根，可重复；不要用 `C:\`、`/` 或整个 home 作为 workspace，除非你明确接受这个暴露面。
- 只传 `--root`、不传 `--workspace` 时，第一个 root 作为工作区，其余作为额外根（兼容旧脚本）。

查看/切换工作区（切换后需重启 daemon）：

```bash
termio workspace show
termio workspace set "$HOME/projects/other"
termio workspace reset
```

### 前台运行

```bash
termio run --config /path/to/config.json
```

`Ctrl+C` 会优雅关闭：取消执行中的调用，发送 WebSocket close frame。

## 自启动

自启动是**用户级**的，目的是读到当前用户的文件。

```bash
termio install --config /absolute/path/config.json
termio status
termio start
termio stop
termio uninstall
```

### Linux

- 写入 `~/.config/systemd/user/termio.service`
- `systemctl --user enable --now termio.service`
- 开机无登录运行：`loginctl enable-linger $USER`
- 没有 user systemd 会话时会报错；需要在登录会话中安装，或改用其他 supervisor

### macOS

- 写入 `~/Library/LaunchAgents/com.termio.agent.plist`
- `launchctl bootstrap gui/<uid> <plist>`
- 需要 GUI 登录会话；SSH 会话中可能失败
- 受 TCC 保护的目录需要给二进制授予 **Full Disk Access**
- 日志：`~/.lares/.termio/termio.out.log` / `.err.log`

### Windows

- 优先创建计划任务 `TermioAgent`（ONLOGON，当前用户）
- 非管理员安装失败时，回退到 HKCU `Software\Microsoft\Windows\CurrentVersion\Run`
- 发布构建的 `termio-agent.exe` 是 `-H=windowsgui`，无控制台窗口；`termio install` 会优先找同目录的 `termio-agent.exe` 或 `termio-agent-windows-<arch>.exe`
- 计划任务在下次登录时启动；`termio start` 可立即启动
- HKCU 回退模式下 `termio start` 不可用，注销重登即可
- **不要**改成 Windows Service（LocalSystem）：它读不到当前用户文件

## 参考网关

```bash
termio-gateway \
  --addr 127.0.0.1:8787 \
  --token dev-token \
  --heartbeat 20s \
  --call-timeout 30s \
  --log-level info
```

按设备 token：

```json
[
  {"id": "dev_aaa", "token": "token-a"},
  {"id": "dev_bbb", "token": "token-b"}
]
```

```bash
termio-gateway --addr 0.0.0.0:8787 --devices-file ./devices.json
```

- 共享 token 与设备 token 可同时配置：共享 token 授权任意设备，设备 token 授权对应设备。
- 网关是单实例、内存态；重启会丢失会话，地端会自动重连。
- `GET /healthz` 无需鉴权。
- 设备不在线时 HTTP 调用立即返回 404 / `-32011`。

### TLS

参考网关不直接提供 TLS。用 Caddy 终止 TLS 的最简配置：

```caddyfile
gateway.example.com {
    reverse_proxy 127.0.0.1:8787
}
```

Caddy 会自动处理 WebSocket 升级和证书。Nginx 需要显式转发 `Upgrade`/`Connection` 头，并把 `proxy_read_timeout` 设为大于心跳间隔（例如 120s）。

## 日志

- 地端：启动、注册、重连、连接错误、工具调用元数据。
- 网关：设备连接/断开、工具名、callId、状态、耗时；debug 级别记录进度。
- 不记录 token、Authorization、工具参数或文件内容。
- 没有内置轮转；长期运行建议用 logrotate（Linux）、launchd 日志管理（macOS）或 Windows 日志策略。

## 升级与回滚

1. 用新版本替换二进制。
2. 前台运行：`Ctrl+C` 后重新启动。
3. 自启动：`termio stop && termio start`（没有单独的 restart 命令）。
4. 回滚：换回旧二进制并重启。旧配置通常能被新版本读取；但新版本写入的新字段在降级后会被旧版本当作未知字段拒绝，降级前请先移除新字段。
5. 地端重启会丢失幂等缓存；升级窗口内的重复 callId 可能重新执行。

## 测试

```bash
go test -count=1 ./...
go vet ./...
gofmt -l .          # 应无输出
go test -count=1 -run TestE2E -v ./internal/gateway
```

E2E 覆盖：注册、搜索/读取、越界拒绝、未知工具、重复 callId 重放、工具超时、客户端断开取消、断线重连、鉴权。

本机 `CGO_ENABLED=0` 时 `-race` 不可用；CI 建议在 Linux 上跑 race。

## 排障

| 现象 | 排查 |
|---|---|
| 网关返回 401 | token 不匹配；检查 `Authorization` 与网关 `--token`/`--devices-file` |
| 设备不在线 / 404 | 地端进程是否运行；`gateway_url` 是否可达；反向代理是否支持 WebSocket |
| 反复重连 / 4002 | 协议版本不匹配；升级网关和地端到同一版本 |
| 反复 4003 | 同一 device_id 有两个进程；停掉旧进程，或更换 device_id；冷却 5 分钟 |
| 调用超时 | 工具是否忽略 ctx；`call_timeout_ms` 是否太小；大目录搜索是否超过预算 |
| `path is outside the allowed roots` | 绝对路径不在 workspace/额外根内；或路径被符号链接解析到边界外 |
| `relative path escapes the workspace` | 相对路径用了 `..` 跳出工作区；改用绝对路径并指向额外根，或调整工作区 |
| 工作区不存在/不可读 | `termio workspace show` 检查；`termio workspace set <已存在目录>` 或 `workspace reset`；重启 daemon |
| 权限拒绝 | macOS Full Disk Access；Windows Controlled Folder Access/AV；Linux ACL/SELinux；确认 workspace/root 可读 |
| `plaintext ws:// is only allowed for loopback` | 远程网关必须用 `wss://` |
| 端口占用 | 换 `--addr`，或停掉旧网关 |
| Windows 登录时闪窗口 | 使用 `termio-agent.exe` 自启动 |
| `systemctl --user` 失败 | 没有 user bus；登录会话中安装，或 `loginctl enable-linger` |
| `launchctl bootstrap` 失败 | 没有 GUI 会话；在桌面登录后安装 |
| 日志为空 | GUI agent 没有 stderr；确保 `log_file` 已配置且目录可写 |

## 健康检查

- `GET /healthz`：进程存活和当前设备数。
- `GET /v1/devices`：需要 token；用于确认设备注册和工具列表。
- 地端没有本地 HTTP 端口；通过网关观察，或看日志。

# 调试与手动联调

本文覆盖两件事：

1. 用 VS Code 断点调试网关和地端。
2. 用 Postman 主动“推消息”、接收“推送”，包括把 Postman 当成一台假设备。

> 只想在终端里输入 JSON 快速自测？先看 [self-test.md](self-test.md) 的 `termioctl` REPL。

## 1. VS Code 调试

### 前置

- VS Code + Go 扩展（`golang.go`）
- Go 1.24+
- Delve（Go 扩展会提示安装；也可手动 `go install github.com/go-delve/delve/cmd/dlv@latest`）

仓库已包含：

```
.vscode/launch.json   # Gateway / Daemon / 组合调试
.vscode/tasks.json    # 自动生成 .debug/config.json、测试、构建
.vscode/extensions.json
```

`.debug/` 和其中的开发 token 已加入 `.gitignore`。

### 启动组合调试

1. 打开 Termio 目录。
2. 按 `F5`，选择 **Termio: Gateway + Daemon**。
3. 两个配置会并行启动：
   - Gateway：`127.0.0.1:8787`，token `dev-token`，日志级别 debug。
   - Daemon：读取 `.debug/config.json`，device ID `dev_debug`，默认工作区为当前仓库；可手工添加更多工作区后用 `Termio Daemon (no init)` 启动。
4. Daemon 启动前会执行 `termio: prepare debug config`，自动生成：

```json
{
  "gateway_url": "ws://127.0.0.1:8787/ws",
  "device_id": "dev_debug",
  "device_name": "debug-device",
  "token": "dev-token",
  "workspaces": [
    {
      "id": "ws_default",
      "name": "default",
      "path": "<workspace>",
      "extra_roots": []
    }
  ],
  "default_workspace_id": "ws_default",
  "max_concurrent_calls": 4,
  "call_timeout_ms": 30000,
  "log_level": "debug",
  "log_file": "<workspace>/.debug/termio.log"
}
```

- 组合调试会先执行一次初始化任务；单独启动 **Termio Daemon** 也会先初始化。
- 每次初始化都会覆盖 `.debug/config.json`（`--force`）。
- 想保留现场或测试多工作区，先手工编辑 `.debug/config.json`，再使用 **Termio Daemon (no init)**；否则 task 会把配置重置成单工作区。
- 不走 VS Code task 时，也可以运行 `make debug-init`（Windows 无 make 时直接跑 tasks.json 里那条 `go run ... init ...` 命令）。
- 两个进程同时启动时，daemon 可能先于 gateway 连接失败；它会 1 秒后重试，属正常现象。

### 建议断点

| 文件 | 位置 | 用途 |
|---|---|---|
| `internal/gateway/gateway.go` | `handleCall` | 看 HTTP 调用如何变成 WS 帧 |
| `internal/gateway/session.go` | `request` | 看 pending call 注册与等待 |
| `internal/gateway/session.go` | `readLoop` / `deliver` | 看地端响应如何配对 |
| `internal/daemon/daemon.go` | `register` | 看注册参数和返回 |
| `internal/daemon/daemon.go` | `readLoop` | 看原始 `data` 帧、方法分发 |
| `internal/daemon/workspace.go` | `LoadWorkspaces` / `Get` | 看每个工作区的 sandbox/registry 如何创建与选择 |
| `internal/daemon/dispatcher.go` | `handleCall` | 看工作区路由、超时、取消、按工作区/会话/callId 缓存 |
| `internal/tools/search_files.go` | `Call` | 看搜索参数、深度、结果 |
| `internal/tools/read_file.go` | `Call` | 看截断、二进制判定 |
| `internal/tools/sandbox.go` | `Resolve` | 看路径边界判断 |

条件断点示例：

- `handleCall`：`params.Name == "search_files"`
- `handleCall`：`params.Meta.WorkspaceID == "ws_docs"`
- `readLoop`：`msg.Method == "tools/call"`
- `dispatch`：`params.Meta.CallID == "debug-call-1"`

调试 Go 时可以在 **Watch** 里查看 `params`、`msg`、`data`、`callCtx`、`result`。不要把 token 打印到日志或截图里。

### 日志

- Daemon：`.debug/termio.log`，同时输出到调试终端。
- Gateway：调试终端 + 可选 `--log-file`。
- 想看原始帧，除了断点，也可以临时在 `readLoop` 的 `json.Unmarshal` 后加 debug 日志；提交前删掉，避免把参数/敏感路径写进日志。

## 消息方向速查

| 方向 | 消息 | 在哪里观察 |
|---|---|---|
| 云端 → 网关 | `POST /v1/devices/{id}/tools/call` | Postman HTTP |
| 网关 → 地端 | `tools/call`、`notifications/cancelled` | 地端断点；假设备模式下在 Postman WebSocket |
| 地端 → 网关 | `notifications/progress`、最终 result | 网关 debug 日志；假设备模式下由你在 WebSocket 手动发 |
| 网关 → 云端 | HTTP response（`CallToolResult` 或 error） | Postman HTTP |

“主动推消息”通常是：云端发 HTTP → 网关通过 WebSocket 推 `tools/call`。
“接消息”通常是：地端回 result → 网关把 HTTP 响应返回。
如果要在 Postman 里**亲眼看到** WS 推送，就让 Postman 扮演地端（第 3 节）。

## 2. Postman HTTP 联调

### 导入集合

导入 [postman/termio.postman_collection.json](postman/termio.postman_collection.json)。

集合变量：

| 变量 | 默认值 | 说明 |
|---|---|---|
| `baseUrl` | `http://127.0.0.1:8787` | 网关地址 |
| `token` | `dev-token` | 与网关 `--token` 一致 |
| `deviceId` | `dev_debug` | 目标设备 |
| `workspaceId` | `ws_default` | 默认工作区 ID |
| `sessionId` | `debug-session` | 调试会话标签 |

先执行 **List devices**（`GET /v1/devices`）。它的 Tests 脚本会把第一台设备 ID 和默认工作区写入集合变量 `deviceId` / `workspaceId`。再执行 **List workspaces**（`GET /v1/devices/{id}/workspaces`）查看所有已注册工作区；只有 ID/名称，没有本地路径。

然后依次试：

- **Search files**：`search_files`，搜索 `*.go`
- **Read file**：`read_file`，读取工作区里的 `go.mod`
- **System info**：`system_info`
- **Duplicate call_id**：同一个 `call_id` 调两次；第二次应命中地端缓存
- **Outside root**：读取 root 外路径，应返回 HTTP 200 + `isError: true`
- **Workspace call**：带 `workspace_id`/`session_id` 调 `system_info`，结果 `_meta` 应回传这两个值
- **Unknown workspace**：应返回 400 + `-32012`
- **Unknown tool**：应返回 400 + `-32602`
- **Missing auth**：应返回 401

集合里的 `readPath` 默认指向本仓库的 `go.mod`，`outsidePath` 默认是 Windows 的 `C:/Windows/win.ini`；换机器或系统时在 Collection Variables 里改成实际路径（Linux/macOS 可用 `/etc/hosts` 测越界）。如果使用假设备（下一节），把 `deviceId` 手动改成 `dev_postman`。

## 3. 把 Postman 当成假设备：主动推消息、接消息

真实链路里，**只有网关能向地端 WebSocket 推送 `tools/call`**。Postman 默认是 HTTP 客户端，看不到这条 WS 流量。

要让 Postman“接推送”，就让 Postman 扮演一台地端设备：

- 用 WebSocket 请求连上网关
- 发送 `termio/register`
- 之后网关会把云端调用推给你
- 你在 WebSocket 里手动回复结果，HTTP 请求就会拿到响应

### 3.1 启动网关

```bash
go run ./cmd/termio-gateway --addr 127.0.0.1:8787 --token dev-token --log-level debug
```

或使用 VS Code 的 **Termio Gateway** 配置。

### 3.2 连接 WebSocket

Postman：**New → WebSocket**。

- URL：`ws://127.0.0.1:8787/ws`
- Headers：
  - `Authorization: Bearer dev-token`
  - `X-Termio-Device: dev_postman`

点击 **Connect**。

> 使用 `dev_postman`，不要和真实 daemon 的 `dev_debug` 冲突；同一 device_id 会 last-wins，后连的会把前一个踢掉（4003）。

### 3.3 发送注册帧

在 WebSocket 消息框发送：

```json
{
  "jsonrpc": "2.0",
  "id": "reg-1",
  "method": "termio/register",
  "params": {
    "protocol_version": 1,
    "mcp_version": "2025-06-18",
    "device": {
      "id": "dev_postman",
      "name": "postman-fake",
      "os": "test",
      "arch": "test",
      "version": "0"
    },
    "tools": [
      {
        "name": "echo",
        "description": "Echo the message back",
        "inputSchema": {
          "type": "object",
          "properties": {"message": {"type": "string"}},
          "required": ["message"]
        }
      }
    ],
    "workspaces": [
      {"id": "ws_default", "name": "default"},
      {"id": "ws_docs", "name": "docs"}
    ],
    "default_workspace_id": "ws_default",
    "max_inflight": 4
  }
}
```

应收到：

```json
{
  "jsonrpc": "2.0",
  "id": "reg-1",
  "result": {
    "protocol_version": 1,
    "mcp_version": "2025-06-18",
    "session_id": "sess_...",
    "server_time": "...",
    "heartbeat_interval_ms": 20000,
    "call_timeout_ms": 30000,
    "max_inflight": 4
  }
}
```

此时 `GET /v1/devices` 应该能看到 `dev_postman`、`echo`，以及 `ws_default`/`ws_docs`；`GET /v1/devices/dev_postman/workspaces` 能直接返回工作区列表。

### 3.4 主动推消息：从 HTTP 触发一次调用

在 Postman 另开一个 HTTP 请求：

```http
POST http://127.0.0.1:8787/v1/devices/dev_postman/tools/call
Authorization: Bearer dev-token
Content-Type: application/json
```

```json
{
  "name": "echo",
  "arguments": {"message": "hello from http"},
  "timeout_ms": 30000,
  "call_id": "debug-call-1",
  "workspace_id": "ws_docs",
  "session_id": "debug-session"
}
```

点击 **Send**。HTTP 会挂起等待，同时 WebSocket 标签页收到网关推送：

```json
{
  "jsonrpc": "2.0",
  "id": "g_c0ffee...",
  "method": "tools/call",
  "params": {
    "name": "echo",
    "arguments": {"message": "hello from http"},
    "_meta": {
      "termio/callId": "debug-call-1",
      "termio/workspaceId": "ws_docs",
      "termio/sessionId": "debug-session",
      "termio/timeoutMs": 30000
    }
  }
}
```

这就是**云端主动推给地端的消息**。`id` 是网关生成的逐跳 ID；`callId` 是端到端幂等键；`workspaceId`/`sessionId` 决定它在哪个工作区和会话上下文执行。省略 `workspace_id` 时，网关填入注册时的默认工作区 `ws_default`。

### 3.5 接消息并回复

在 WebSocket 里回复，**`id` 必须与推送帧完全一致**：

```json
{
  "jsonrpc": "2.0",
  "id": "g_c0ffee...",
  "result": {
    "content": [{"type": "text", "text": "echo: hello from http"}],
    "structuredContent": {"echo": {"message": "hello from http"}},
    "isError": false,
    "_meta": {"termio/callId": "debug-call-1"}
  }
}
```

切回 HTTP 标签页，请求返回上面的 `CallToolResult`。

> 手动复制时注意替换 `g_...` 和 `callId`，Postman 不会自动帮你关联。

### 3.6 进度通知

在回复最终结果之前，先发一条进度：

```json
{
  "jsonrpc": "2.0",
  "method": "notifications/progress",
  "params": {
    "progressToken": "",
    "progress": 1,
    "total": 2,
    "message": "working",
    "_meta": {
      "termio/callId": "debug-call-1",
      "termio/workspaceId": "ws_docs",
      "termio/sessionId": "debug-session"
    }
  }
}
```

网关会在 debug 日志里打印 `tool progress`；HTTP 请求仍在等待。然后再发 3.5 的结果。

### 3.7 超时与取消

发一个 HTTP 调用，`timeout_ms: 1000`，但在 WebSocket 里**不要回复**。

- 网关等待约 3 秒（`timeout + 2s`）
- HTTP 返回 504
- WebSocket 收到：

```json
{
  "jsonrpc": "2.0",
  "method": "notifications/cancelled",
  "params": {
    "requestId": "g_c0ffee...",
    "reason": "client_cancelled",
    "_meta": {
      "termio/callId": "debug-call-1",
      "termio/workspaceId": "ws_docs",
      "termio/sessionId": "debug-session"
    }
  }
}
```

真实地端收到 cancel 后会取消执行并回 `-32800`；Postman 假设备可以直接忽略。

> 完整链路（启动 → 注册 → HTTP 入站 → 工作区路由 → 工具 → 进度/取消 → 响应）的逐函数断点表见 [debug-full-chain.md](debug-full-chain.md)。

### 3.8 工具失败

回复一个 `isError: true` 的结果：

```json
{
  "jsonrpc": "2.0",
  "id": "g_c0ffee...",
  "result": {
    "content": [{"type": "text", "text": "simulated failure"}],
    "isError": true,
    "_meta": {"termio/callId": "debug-call-1"}
  }
}
```

HTTP 状态仍是 200，body 里 `isError: true`。这就是 Agent 应该读到的工具失败。

### 3.9 心跳

网关每 20 秒 ping。Postman 的 WebSocket 客户端通常会处理 ping/pong；如果连接被回收，重新 Connect 并重发注册帧即可。超过 20 秒无响应，网关可能断开连接。

## 4. 真实地端的推/收流程

使用 **Termio: Gateway + Daemon** 组合调试，然后：

1. Postman 调 `POST /v1/devices/dev_debug/tools/call`。
2. 网关通过 WebSocket 推 `tools/call` 给真实 daemon。
3. daemon 执行工具，发 progress/结果。
4. 网关把结果作为 HTTP 响应返回。

在 Postman 里只能看到第 1 步和第 4 步；第 2/3 步在内部。要观察：

- 在 `internal/daemon/daemon.go` `readLoop` 的 `conn.Read` 后打断点，查看 `data`。
- 在 `internal/gateway/session.go` `request`/`deliver` 打断点，查看 pending 配对。
- 看 daemon 日志 `.debug/termio.log` 和 gateway debug 日志。

## 5. 常见问题

| 现象 | 处理 |
|---|---|
| Postman WebSocket 连接 401 | token 与网关不一致；检查两个 Header |
| 注册后没有收到推送 | HTTP 请求的 deviceId 是否写成了 `dev_postman`；WS 是否仍连接 |
| 推送的 `id` 对不上 | 复制完整 `g_...`，不要把 `id` 和 `callId` 搞混 |
| HTTP 一直挂起 | 是否在 WS 里回复了**同一个 id**；是否超时 |
| 连接过一会儿断开 | 心跳/代理超时；重连并重新注册 |
| 同一设备反复 4003 | 真实 daemon 和 Postman 用了同一个 deviceId；换一个 |
| 端口 8787 被占用 | 修改 launch.json 的 `--addr`、tasks.json 的 `--url`、Postman 的 `baseUrl`，保持三处一致 |
| 搜索返回 root 外错误 | 这是沙箱在正常工作；确认调用的 `workspace_id` 与目标目录一致 |
| 修改配置后没生效 | `Termio Daemon` 每次会 `--force` 重建；用 `Termio Daemon (no init)` 或改 tasks.json |
| Windows 调试器找不到 dlv | 安装 Go 扩展，或 `go install github.com/go-delve/delve/cmd/dlv@latest` |

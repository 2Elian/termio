# 自测端子

目标：启动真实网关 + 真实地端 daemon，然后从终端用 JSON 调工具，验证整条链路。

Termio 不在端子上开监听端口；地端只主动连接网关。所以自测入口是**网关的 HTTP API**。`termioctl` 是专门做这件事的终端 JSON REPL；它通过 HTTP 调网关，网关再通过 WebSocket 把调用转发给真实 daemon。

> 这不是 gRPC。设备协议是 JSON-RPC over WebSocket；`termioctl` 只是把同一套 JSON 工具调用包装成终端交互。

## 1. 启动网关

```bash
go run ./cmd/termio-gateway --addr 127.0.0.1:8787 --token dev-token --log-level debug
```

## 2. 启动地端 daemon

方式 A：默认配置

```bash
termio init --token dev-token
termio run
```

默认工作区是 `~/.lares`，配置在 `~/.lares/.termio/config.json`。

再注册一个本地工作区，用于测试跨工作区路由（需重启 daemon）：

```bash
mkdir -p "$HOME/termio-docs"
termio workspace add --id ws_docs --name docs "$HOME/termio-docs"
termio workspace list
# 重启 termio run / VS Code 调试配置
```

方式 B：调试配置（VS Code）

```bash
termio run --config .debug/config.json
```

或直接按 `F5` 选择 **Termio: Gateway + Daemon**。

## 3. 启动终端 REPL

```bash
go run ./cmd/termioctl --gateway http://127.0.0.1:8787 --token dev-token
# 或构建后
./bin/termioctl --gateway http://127.0.0.1:8787 --token dev-token
# Windows: .\bin\termioctl.exe ...
```

- 不传 `--device` 时自动使用第一台在线设备；也可以用 `--device dev_debug` 指定。
- 不传 `--token` 时依次读取 `TERMIO_TOKEN`、`dev-token`。
- `--workspace ws_docs` 设置 REPL 的默认工作区；`--session chat-1` 设置会话 ID。
- 如果 stdin 不是 TTY（例如管道），默认按行读取 JSON；要强制 REPL 加 `--interactive`。

启动后直接输入 JSON，每行一个对象：

```json
{"name":"system_info","arguments":{}}
```

```json
{"name":"search_files","arguments":{"pattern":"*.go","max_results":10}}
```

```json
{"name":"read_file","arguments":{"path":"go.mod"}}
```

```json
{"name":"read_file","arguments":{"path":"../outside"}}
```

```json
{"name":"read_file","arguments":{"path":".termio/config.json"}}
```

```json
{"name":"system_info","arguments":{},"workspace_id":"ws_docs","session_id":"chat-42"}
```

```json
{"name":"read_file","arguments":{"path":"notes.md"},"workspace_id":"ws_docs","session_id":"chat-42"}
```

预期：

- 前三个正常返回 `CallToolResult`。
- `../outside` 返回 `relative path escapes the workspace`（或越界错误）。
- `.termio/config.json` 返回 `path is reserved for Termio`，HTTP 仍是 200 + `isError: true`。
- 带 `workspace_id` 的调用在对应工作区执行；结果的 `_meta` 会带回 `termio/workspaceId` 和 `termio/sessionId`。
- 未知 `workspace_id` 返回 HTTP 400 + `-32012`。

也支持 JSON-RPC 形状的 envelope：

```json
{"jsonrpc":"2.0","id":"1","method":"tools/call","params":{"name":"system_info","arguments":{},"_meta":{"termio/workspaceId":"ws_docs","termio/sessionId":"chat-42"}}}
```

## 4. REPL 命令

| 命令 | 作用 |
|---|---|
| `:help` | 帮助 |
| `:devices` | 列出在线设备 |
| `:device <id>` | 切换设备 |
| `:tools` | 查看当前设备的工具 |
| `:workspaces` | 查看当前设备已注册的工作区 |
| `:workspace <id>` | 切换当前工作区；`:workspace -` 回到设备默认 |
| `:session <id>` | 设置会话 ID；`:session -` 清除 |
| `:health` | 网关健康检查 |
| `:timeout <ms>` | 设置默认工具超时 |
| `:quit` | 退出 |

## 5. 非交互模式

```bash
# 列设备
./bin/termioctl --list

# 列工具
./bin/termioctl --tools --device dev_debug

# 单次调用
./bin/termioctl --device dev_debug \
  --json '{"name":"system_info","arguments":{}}'

# 指定工作区和会话
./bin/termioctl --device dev_debug --workspace ws_docs --session chat-42 \
  --json '{"name":"read_file","arguments":{"path":"notes.md"}}'

# 管道：每行一个 JSON；有失败时退出码非 0
printf '%s\n%s\n' \
  '{"name":"system_info","arguments":{}}' \
  '{"name":"read_file","arguments":{"path":"go.mod"}}' \
  | ./bin/termioctl --device dev_debug
```

## 6. 幂等与重试

给同一个逻辑调用固定 `call_id`：

```json
{"name":"system_info","arguments":{},"call_id":"selftest-1"}
```

再发一次相同请求。第二次应该命中地端缓存并直接返回，不会重新执行。修改 `arguments` 但复用同一 `call_id` 会返回参数错误（callId 冲突）。

幂等键是 `(workspace_id, session_id, call_id)`：

- 同一个 `call_id` 在 `ws_default` 和 `ws_docs` 是两个独立调用，都会真正执行。
- 同一工作区、不同 `session_id` 也互不重放。
- 只有三元组完全一致时才会重放缓存。

## 7. Postman

导入 [`docs/postman/termio.postman_collection.json`](postman/termio.postman_collection.json)：

1. 设置 `baseUrl` = `http://127.0.0.1:8787`，`token` = 网关 token。
2. 先跑 **List devices (run first)**，测试脚本会自动写入 `deviceId`、默认 `workspaceId`，并在有第二个工作区时写入 `otherWorkspaceId`。
3. 跑 **Refresh tools** 触发一次真实 `tools/list`。
4. 跑 **List workspaces** 查看全部已注册工作区。
5. 依次跑 Search files / Read file / System info / Workspace call / Unknown workspace / Outside allowed root / Unknown tool / Missing auth。

Postman 也可以扮演假设备，手动接收网关推送的 `tools/call` 并回复：

```text
WebSocket ws://127.0.0.1:8787/ws
Authorization: Bearer dev-token
X-Termio-Device: dev_postman
```

完整帧教程见 [debugging.md](debugging.md)。

## 8. 常见问题

| 现象 | 处理 |
|---|---|
| `no online devices` | daemon 没启动，或网关 URL/token 不对；先看网关 `/v1/devices` |
| 401 | token 与网关 `--token`/`--devices-file` 不一致 |
| 404 | `deviceId` 不存在或设备已断开 |
| 400 + `-32012` | `workspaceId` 未注册；`termio workspace list` 检查，新增后重启 daemon |
| `path is reserved for Termio` | 正常：`.termio` 是端子私有目录，工具不能访问 |
| 工具返回 `isError: true` | 工具执行失败；看 `content[0].text`，不是网络错误 |
| 超时 | 加大 `--timeout`/`:timeout`，或检查工具是否忽略 ctx |
| REPL 直接退出 | stdin 不是 TTY；加 `--interactive`，或使用 `--json` |
| 想看进度 | v1 网关只写 debug 日志；用 `--log-level debug` 观察 |
| 想测取消/网关超时 | 用 [debugging.md](debugging.md) 的 Postman 假设备流程，手动不回复，观察 504 和 cancelled 帧 |

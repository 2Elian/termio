# 全链路调试：启动 → 消息 → 工作区 → 工具 → 响应

本文按真实执行顺序，把 Termio 从**进程启动**到**一条工具调用返回**的每一段拆开：

- 每个功能点由哪个文件、哪个函数负责；
- 应该在哪里下断点、条件断点怎么写、断下来看什么变量；
- 多工作区、多会话、并发、幂等、超时、取消分别怎么验证；
- 哪些行为有自动化测试覆盖，哪些需要手工观察。

> 只想快速跑通，先看 [self-test.md](self-test.md)；想看 Postman 假设备/原始帧，看 [debugging.md](debugging.md)。本文是“逐点调试手册”。

---

## 0. 调试拓扑

```
termioctl / curl / Postman
        │ HTTP :8787
        ▼
termio-gateway  ── WebSocket（地端主动外连）──▶  termio daemon
                                                      ├─ Workspace ws_default
                                                      │    ├─ os.Root
                                                      │    └─ Registry
                                                      └─ Workspace ws_docs
                                                           ├─ os.Root
                                                           └─ Registry
```

三个进程角色：

| 进程 | 入口 | 作用 |
|---|---|---|
| `termio-gateway` | `cmd/termio-gateway/main.go` | 持有设备 WS、HTTP API、pending call |
| `termio` | `cmd/termio/main.go` | 地端 daemon：配置、工作区、沙箱、调度 |
| `termioctl` | `cmd/termioctl/main.go` | 终端 JSON 客户端（可选） |

关键事实：

- 一次调用会经过 **两个进程、三条边界**：HTTP → 网关内存 → WebSocket → daemon 调度器 → 工具沙箱。
- `workspaceId` 在网关侧被补默认值，在 daemon 侧被解析成具体的 `*daemon.Workspace`。
- 每个工作区有自己的 `Sandbox`、`Registry`、并发信号量；会话只是 `_meta` 中的标签。
- 结果、进度、取消都会带回 `workspaceId`/`sessionId`。

---

## 1. 最短调试路径（先做这一步）

不需要手工 `init`，也不需要自己 `workspace add`。VS Code 的预启动任务会调用 `go run ./scripts/prepare-debug`，自动生成：

- `.debug/config.json`：`ws_default`（仓库根）+ `ws_docs`（`.debug/ws-docs`）；
- `.debug/ws-docs/notes.md`：跨工作区测试文件；
- 网关地址、`dev_debug` 设备 ID、`dev-token`、debug 日志路径。

### 1.1 方式 A：一键启动网关 + daemon

1. VS Code 打开 `E:\Project\Termio`。
2. `F5`，选择 **Termio: Gateway + Daemon**。
3. 两个进程都会在 Delve 下启动：
   - Gateway：`127.0.0.1:8787`，token `dev-token`，debug 日志；
   - Daemon：读取自动生成的 `.debug/config.json`，两个工作区。
4. 在 `internal/daemon/dispatcher.go:handleCall`、`internal/gateway/gateway.go:handleCall` 等位置下断点。
5. 用 Postman 发请求（见 1.3）。

### 1.2 方式 B：PowerShell 起网关 + VS Code 调试 daemon（你描述的方式）

完全可行，推荐这样分工：网关不常改，用 PowerShell；daemon 是调试重点，用 F5。

**PowerShell：**

```powershell
cd E:\Project\Termio
go run ./cmd/termio-gateway --addr 127.0.0.1:8787 --token dev-token --log-level debug
```

**VS Code：**

1. `F5`，选择 **Termio Daemon**。
2. 它会先执行 `termio: prepare debug config`（自动生成双工作区配置），再在 Delve 下启动 daemon。
3. 断点命中后，HTTP 请求会停在 daemon 的处理链路上。

> 只有你想保留自己手改的 `.debug/config.json` 时，才用 **Termio Daemon (no init)**。`Termio Daemon` 和组合配置都会先重置成标准双工作区环境。

预期 daemon 日志：

```text
termio starting ... default_workspace=ws_default workspace=<repo> workspaces=2 tools=3
registered with gateway device_id=dev_debug tools=3 workspaces=2 heartbeat=20s
```

### 1.3 Postman 最短请求序列

导入 [`postman/termio.postman_collection.json`](postman/termio.postman_collection.json)，变量默认：

```text
baseUrl    http://127.0.0.1:8787
token      dev-token
deviceId   dev_debug
workspaceId ws_default
```

按顺序跑：

1. **List devices (run first)**：脚本写入 `deviceId`、默认 `workspaceId`，第二个工作区写入 `otherWorkspaceId`（应该是 `ws_docs`）。
2. **List workspaces**：确认 `ws_default` + `ws_docs`，且没有本地路径。
3. **System info**：默认工作区；结果 `_meta.termio/workspaceId=ws_default`。
4. **Workspace call (explicit workspace)**：`ws_docs` + 独立 session；结果 `_meta` 应回传 `ws_docs`。
5. **Read file**：默认读仓库 `go.mod`；想读 docs 工作区，把 body 改成：
   ```json
   {"name":"read_file","arguments":{"path":"notes.md"},"workspace_id":"ws_docs","session_id":"postman-session"}
   ```
   应返回 `hello from docs workspace`。
6. **Unknown workspace**：400 + `-32012`。
7. **Unknown tool**：400 + `-32602`。
8. **Missing auth**：401。

### 1.4 验证注册结果（可选）

```bash
curl -H "Authorization: Bearer dev-token" http://127.0.0.1:8787/v1/devices
curl -H "Authorization: Bearer dev-token" http://127.0.0.1:8787/v1/devices/dev_debug/workspaces
```

应看到两个工作区，且**没有** `E:\...` 路径。路径泄露应视为 bug。

### 1.5 命令行 REPL（可选）

```bash
go run ./cmd/termioctl \
  --gateway http://127.0.0.1:8787 --token dev-token \
  --device dev_debug --workspace ws_default --session debug-session
```

它只是把 Postman 的第一步换成终端输入，后续链路完全相同。

### 1.6 手工配置（只在需要自定义时）

自动配置不满足时，可以自己做，然后用 **Termio Daemon (no init)** 避免被覆盖：

```bash
mkdir -p .debug/ws-docs
go run ./cmd/termio init --config .debug/config.json \
  --url ws://127.0.0.1:8787/ws --token dev-token \
  --device-id dev_debug --device-name debug-device \
  --workspace "$PWD" --log-file "$PWD/.debug/termio.log" --log-level debug --force
go run ./cmd/termio workspace add --config .debug/config.json \
  --id ws_docs --name docs .debug/ws-docs
```

`.debug/config.json` 会包含 `workspaces` 数组和 `default_workspace_id`，`workspace list` 应显示两行。

---

## 2. 启动链路：进程如何起来

### 2.1 网关启动

代码路径：

```
cmd/termio-gateway/main.go:main
  → flag 解析（--addr/--token/--devices-file/--log-level/--log-file）
  → gateway.New(gateway.Options{...})
      → 校验 token
      → 构造 hub
      → 注册路由：
           GET  /healthz
           GET  /ws
           GET  /v1/devices
           GET  /v1/devices/{id}/tools
           GET  /v1/devices/{id}/workspaces
           POST /v1/devices/{id}/tools/call
  → http.Server.ListenAndServe
```

断点：

| 文件 / 函数 | 观察 |
|---|---|
| `internal/gateway/gateway.go:New` | `opts.Token`、`opts.Devices`、超时/心跳是否被夹到合法区间 |
| `internal/gateway/gateway.go:handleHealth` | `devices` 数量（启动时应为 0） |

### 2.2 daemon 启动：配置 → 工作区 → 沙箱

代码路径：

```
cmd/termio/main.go:run
  → runDaemon
      → loadConfig
          → config.LoadDefault / config.Load
              → applyDefaults
              → normalizeWorkspaces   // 迁移旧 workspace/allowed_roots、规范化路径、查重
              → Validate              // 路径存在、默认工作区存在、并发/超时范围
      → logging.New                   // stderr + 文件
      → daemon.LoadWorkspaces(cfg, path, nil)
          → config.AppDataDir()       // $LARES/.termio，加入 deny 列表
          → for each cfg.Workspaces:
              → tools.NewSandbox(path, extra_roots, deny...)
                  → os.OpenRoot(workspace)
                  → os.OpenRoot(extra root)
              → tools.DefaultRegistry(sandbox)
              → &daemon.Workspace{ID, Name, Sandbox, Registry, Sem}
          → NewWorkspaceSet(defaultID, workspaces...)
      → log.Info("termio starting", ...)
      → daemon.New(cfg, log, workspaces)
      → d.Run(ctx)
```

断点：

| 文件 / 函数 | 条件/观察 |
|---|---|
| `internal/config/config.go:normalizeWorkspaces` | `c.Workspaces` 是否已从旧字段迁移；路径是否被 `EvalSymlinks`；是否检测到重复路径 |
| `internal/config/config.go:Validate` | 每个 `w.Path` 是否存在；`DefaultWorkspaceID` 是否在列表中 |
| `internal/daemon/workspace.go:LoadWorkspaces` | `deny` 是否包含 `$LARES/.termio`、config、log；每个 `sb.Roots()`；`Sem` 容量 = `max_concurrent_calls` |
| `internal/daemon/workspace.go:NewWorkspaceSet` | `set.byID`、`set.order`、`defaultID` |
| `cmd/termio/main.go:runDaemon` | `len(workspaces.List())` 应为 2；`defaultWorkspace.Sandbox.Workspace()` |

要看的变量：

- `cfg.Workspaces`：配置里的 ID/name/path/extra_roots。
- `ws.Sandbox.Roots()`：第一个永远是工作区，后面是额外根。
- `cap(ws.Sem)`：每工作区并发额度。
- `workspaces.List()`：只含 ID/name，不含 path。

常见失败：

- 路径不存在 → `LoadWorkspaces` 返回 `workspace "ws_x": ...`，daemon 启动失败；不会回退。
- 默认工作区 ID 不在列表 → `Validate` 失败。
- 两个工作区指向同一目录 → `normalizeWorkspaces` 报 duplicate path。

### 2.3 注册：daemon → 网关

代码路径：

```
internal/daemon/daemon.go:Run
  → runOnce
      → websocket.Dial(gateway_url, headers)
          Authorization: Bearer <token>
          X-Termio-Device: <device_id>
          User-Agent: termio/<version> (os/arch)
      → register(ctx, conn, writer)
          → protocol.NewRequest(MethodRegister, RegisterParams{
                Tools:              workspaces.Specs(),       // 各工作区工具并集
                Workspaces:         workspaces.List(),       // ID + name
                DefaultWorkspaceID: workspaces.DefaultID(),
                MaxInflight:        cfg.MaxConcurrentCalls,
            })
          → writer.WriteJSON
          → conn.Read(10s)  ← 注册响应
          → 校验 id/error/protocol_version
          → 得到 heartbeat
      → newDispatcher(...)
      → readLoop(...)
```

网关侧：

```
internal/gateway/gateway.go:handleWS
  → 读 X-Termio-Device / token，tokenMatchesDevice
  → websocket.Accept
  → 10s 内读第一帧
  → 校验 jsonrpc/method/id
  → DecodeParams → RegisterParams
  → params.Device.ID == 鉴权设备
  → params.ProtocolVersion == 1
  → normalizeWorkspaceRegistration(params.Workspaces, params.DefaultWorkspaceID)
       - ID 合法、唯一、≤64 个
       - 默认 ID 必须在列表中；缺失则取第一个
  → 构造 RegisterResult（session_id、心跳、超时、max_inflight）
  → 写回响应
  → newDeviceConn(..., workspaces, defaultWorkspaceID, ...)
  → hub.add(dc)              // 同 device 旧连接 4003
  → go dc.heartbeat(...)
  → dc.readLoop()
```

断点：

| 文件 / 函数 | 观察 |
|---|---|
| `internal/daemon/daemon.go:register` | `RegisterParams.Workspaces`、`DefaultWorkspaceID`、`Tools` 并集；响应 `heartbeat` |
| `internal/gateway/gateway.go:handleWS` | `deviceID`、`params.Workspaces`、`workspaces`（规范化后）、`defaultWorkspaceID` |
| `internal/gateway/gateway.go:normalizeWorkspaceRegistration` | 重复 ID、缺失默认值、超过 64 个 |
| `internal/gateway/session.go:newDeviceConn` | `dc.workspaces`、`dc.defaultWorkspaceID`、`dc.maxInflight` |

注册成功后：

- `hub` 中多了一个 `deviceConn`；
- 云端可以 `GET /v1/devices` 看到 `workspaces`/`default_workspace_id`；
- `GET /v1/devices/{id}/workspaces` 可直接取工作区。

---

## 3. 消息入站：HTTP → 网关 → WebSocket

### 3.1 客户端发送

`termioctl`：

```
cmd/termioctl/main.go:send
  → parseCall（普通 JSON 或 JSON-RPC envelope）
  → 合并 client.workspaceID / client.sessionID
  → POST /v1/devices/{deviceId}/tools/call
      {name, arguments, timeout_ms, call_id?, workspace_id?, session_id?}
```

等价 curl：

```bash
curl -X POST "http://127.0.0.1:8787/v1/devices/dev_debug/tools/call" \
  -H "Authorization: Bearer dev-token" \
  -H "Content-Type: application/json" \
  -d '{
    "name":"system_info",
    "arguments":{},
    "workspace_id":"ws_docs",
    "session_id":"chat-1",
    "call_id":"debug-1"
  }'
```

### 3.2 网关处理 HTTP

代码路径：

```
internal/gateway/gateway.go:handleCall
  → PathValue("id")
  → tokenMatchesDevice                      // 401
  → MaxBytesReader(1 MiB)
  → json.Decoder.DisallowUnknownFields       // 拼错字段直接 400
  → name 非空
  → call_id 合法（省略则生成 call_<hex>）
  → hub.get(deviceID)                        // 404 设备不在线
  → workspaceID := body.WorkspaceID
      if workspaceID == "" { workspaceID = dc.defaultWorkspaceID }
  → validWorkspaceRef / validSessionRef      // 400
  → timeout = min(opts.CallTimeout, body.TimeoutMS)，夹在 1s–5min
  → args 空则 {}
  → rpcID := NewID("g_")
  → protocol.NewRequest(rpcID, "tools/call", ToolCallParams{
        Name, Arguments,
        Meta: {
          CallID:        call_id,
          ProgressToken: body.progress_token,
          WorkspaceID:   workspaceID,       // 已补默认
          SessionID:     body.session_id,
          TimeoutMS:     timeout,
        },
    })
  → ctx = r.Context() + timeout + 2s
  → dc.request(ctx, req)
```

`deviceConn.request`：

```
internal/gateway/session.go:request
  → 新建 pending 通道
  → d.pending[msg.ID] = ch
  → writer.WriteJSON(msg)                   // 真正写入设备 WebSocket
  → select { ctx.Done | d.done | ch }
```

断点：

| 文件 / 函数 | 断点条件 | 观察 |
|---|---|---|
| `internal/gateway/gateway.go:handleCall` | `body.Name == "system_info"` | `body.WorkspaceID`、`workspaceID`（补默认后）、`sessionID`、`timeout` |
| `internal/gateway/gateway.go:handleCall` | `workspaceID == "ws_docs"` | 确认路由参数来自 HTTP，不是本地猜的 |
| `internal/gateway/gateway.go:handleCall` | `callID == "debug-1"` | `rpcID` 与 `callID` 不同：前者逐跳，后者端到端 |
| `internal/gateway/session.go:request` | `msg.Params` 包含 `ws_docs` | `d.pending` 是否插入、写 WS 是否成功 |

此时如果在 daemon 的 `readLoop` 断点，应该看到同一个 `rpcID` 和 `_meta`。

### 3.3 网关侧等待与取消

- HTTP 请求挂起，直到：
  - daemon 回 result/error；
  - daemon 断开（`errDeviceClosed` → 502）；
  - `timeout + 2s` 到期（504）；
  - 云端 HTTP 客户端断开（不写响应，发 cancel）。
- `ctx.Err() != nil` 时网关发 `notifications/cancelled`，带 `requestId=rpcID` 以及工作区/会话。

---

## 4. daemon 收消息：读循环与调度器

### 4.1 读循环

代码路径：

```
internal/daemon/daemon.go:readLoop
  → conn.Read(ctx)                         // 单读循环，永不阻塞在工具上
  → 只处理文本帧
  → json.Unmarshal → protocol.Message
  → 校验 jsonrpc == "2.0"、id 长度 ≤256
  → switch:
       "tools/call" + id        → go disp.handleCall(ctx, &msg)
       "tools/list" + id        → go d.handleToolsList(...)
       "notifications/cancelled"→ disp.cancelCall(params)
       其他 request             → -32601 method not found
       其他 response            → debug 日志忽略
```

断点：

| 文件 / 函数 | 条件 | 观察 |
|---|---|---|
| `internal/daemon/daemon.go:readLoop` | `msg.Method == "tools/call"` | `data` 原始帧、`msg.ID`、`msg.Params` |
| `internal/daemon/daemon.go:readLoop` | `msg.Method == "notifications/cancelled"` | `params.RequestID`、`params.Meta` |

> 读循环必须保持非阻塞；如果在这里看到工具执行代码，说明结构被改坏了。

### 4.2 调度器：工作区解析与执行

代码路径（按执行顺序）：

```
internal/daemon/dispatcher.go:handleCall
  1. DecodeParams → ToolCallParams
  2. name 非空
  3. callID 非空、合法（无控制字符/NUL，≤128）
  4. workspaceID = trim(params.Meta.WorkspaceID)
     sessionID   = trim(params.Meta.SessionID)
     校验 sessionID（≤256 可打印字符）
  5. ws, err := d.workspaces.Get(workspaceID)
       - 空 → 默认工作区
       - 未知 → -32012
     workspaceID = ws.ID                  // 归一化为实际 ID
  6. key = callKey(workspaceID, sessionID, callID)  // NUL 分隔三元组
     fingerprint = name + "\x00" + arguments
  7. lookupCache(key, fingerprint, rpcID)
       - 命中且指纹相同 → 直接重放
       - 命中但指纹不同 → -32602
  8. select ws.Sem <- struct{}{}          // 每工作区独立并发
       满 → -32010 "workspace is busy"
  9. callCtx, cancel = WithCancelCause(parent)
     timer = AfterFunc(timeout, cancel(errCallTimeout))
 10. reserve(key, workspaceID, sessionID, callID, fingerprint, rpcID, cancel)
       - 已完成 → reserveCached（重放）
       - 执行中 → reserveDuplicate（-32005）
       - 指纹不同 → reserveMismatch（-32602）
 11. progressSender{token, callID, workspaceID, sessionID}
 12. ws.Registry.Call(callCtx, name, args, progress.send)
 13. cause := context.Cause(callCtx)
       - errShutdown      → 放弃响应
       - errCallCancelled → -32800
       - errCallTimeout   → result = isError("tool timed out ...")
       - parent 关闭      → 放弃响应
 14. ArgError / UnknownToolError → -32602
     其他执行错误 → result.isError=true（仍是 result）
 15. result.Meta = {CallID, WorkspaceID, SessionID}
 16. NewResult → cachePut(key, fingerprint, result) → send
```

断点：

| 文件 / 函数 | 条件 | 观察 |
|---|---|---|
| `internal/daemon/dispatcher.go:handleCall` | `params.Name == "system_info"` | `workspaceID`、`sessionID`、`callID` |
| 同上 | `workspaceID == "ws_docs"` | `ws.ID`、`ws.Sandbox.Workspace()`、`cap(ws.Sem)` |
| 同上 | `callID == "debug-1"` | `key` 是否含空 session；`fingerprint` |
| 同上 | `err != nil`（Delve 支持条件断点但表达式受限，可先断在 `ws.Registry.Call` 后） | `cause`、`callErr` 类型（`*tools.ArgError` / `*tools.UnknownToolError`） |
| `internal/daemon/dispatcher.go:lookupCache` | `key` 包含 `debug-1` | 是否命中、`expires` |
| `internal/daemon/dispatcher.go:reserve` | 同上 | `inflight`、`rpcToCall` |
| `internal/daemon/dispatcher.go:cachePut` | 同上 | 结果是否写入、缓存 key |
| `internal/daemon/dispatcher.go:progressSender.send` | `p.callID == "debug-1"` | 节流时间、progress/total |
| `internal/daemon/dispatcher.go:cancelCall` | `params.RequestID != ""` | 是否通过 `rpcToCall` 找到 key |

**最容易看错的两个变量：**

- `params.Meta.WorkspaceID` 可能为空（客户端省略），而 `workspaceID` 已被 `Get` 补成默认值。
- `key` 是三元组，不是裸 `callID`；同一个 `callID` 在两个工作区会有两个不同的 key。

### 4.3 工作区查找

代码路径：

```
internal/daemon/workspace.go:WorkspaceSet.Get(id)
  if id == "" { id = s.defaultID }
  ws, ok := s.byID[id]
  if !ok → ErrWorkspaceNotFound
```

断点：

| 文件 / 函数 | 观察 |
|---|---|
| `internal/daemon/workspace.go:Get` | `id`、`s.defaultID`、`s.byID` 的键 |
| `internal/daemon/workspace.go:List` | 只返回 `{ID, Name}` |
| `internal/daemon/workspace.go:Specs` | 各工作区工具并集，按名称去重 |

---

## 5. 工具执行：Registry → Sandbox → 工具

### 5.1 Registry

```
internal/tools/registry.go:Registry.Call
  → tools[name]
      不存在 → *UnknownToolError
  → tool.Call(ctx, args, progress)
```

每个工作区的 `Workspace.Registry` 由 `tools.DefaultRegistry(ws.Sandbox)` 构建，因此工具里的 `sandbox` 指针天然绑定该工作区。

断点：

- `internal/tools/registry.go:Call`：`name`、`r` 的地址（可对比两个工作区的 registry 地址）。
- `internal/tools/registry.go:decodeArgs`：参数是否被 `DisallowUnknownFields` 拒绝。

### 5.2 `system_info`

```
internal/tools/system_info.go:Call
  → decodeArgs
  → 检查 ctx
  → 读取 sandbox.Workspace() / sandbox.Roots()
  → textResult(...)
```

断点结果应显示所选工作区的路径：

- `ws_default` → 仓库根目录；
- `ws_docs` → `.debug/ws-docs`。

如果两次调用返回同一个路径，优先检查 daemon 是否用了同一个 `*Workspace`，而不是先怀疑网关。

### 5.3 `search_files`

```
internal/tools/search_files.go:Call
  → decodeArgs（pattern 必填，max_results/max_depth 夹紧）
  → path 为空 → path = t.sandbox.Workspace()
  → t.sandbox.Resolve(path)
       → 相对路径基于该工作区
       → 绝对路径必须在该工作区或其 extra roots
       → 规范化符号链接/8.3 短名
       → deny 检查（.termio/config/log）
  → Stat 必须是目录
  → 构造 nameMatcher（glob 或子串）
  → fs.WalkDir(resolved.Entry.root.FS(), resolved.RelSlash, ...)
       → 每 512 项 progress
       → 跳过 symlink
       → include_hidden=false 时跳过点开头和依赖目录
       → IsDenied 跳过私有目录
       → 最多 max_results（默认 200，上限 1000）
  → textResult(matches + structuredContent)
```

断点：

| 文件 / 函数 | 条件 | 观察 |
|---|---|---|
| `internal/tools/search_files.go:Call` | `args.Pattern == "*.md"` | `args.Path`（空 → 工作区）、`t.sandbox.Workspace()` |
| `internal/tools/sandbox.go:Resolve` | `requestPath` 含 `ws-docs` 或 `.termio` | `canonical`、`entry.display`、`rel`；deny 命中 |
| `internal/tools/search_files.go:Call`（walk 闭包） | `result.Scanned%512 == 0` | 进度是否发出、`p` 路径前缀 |
| `internal/tools/sandbox.go:IsDenied` | 路径含 `.termio` | 返回 true |

### 5.4 `read_file`

```
internal/tools/read_file.go:Call
  → path 必填
  → sandbox.Resolve(path)
  → root.Open(resolved.RelOS)
  → Stat 必须普通文件
  → 读 max_bytes+1（默认 256 KiB，上限 512 KiB）
  → UTF-8 且无 NUL → text；否则 base64
  → structuredContent: path/size/bytes_returned/truncated/is_binary/encoding
```

断点：

- `read_file.go:Call`：`args.Path`、`resolved.Abs`、`resolved.RelOS`。
- 读取后：`isText`、`truncated`、`len(data)`。

跨工作区验证：

- `{"path":"notes.md","workspace_id":"ws_docs"}` → 成功；
- 同样的请求 `workspace_id=ws_default` → 文件不存在（isError），**不会**跑到 docs。

### 5.5 沙箱边界

`internal/tools/sandbox.go:Resolve` 是路径安全的核心：

1. 空路径 → 工作区；
2. 相对路径 → `filepath.Join(workspace, clean)`，`..` 不能逃出；
3. 绝对路径 → 校验盘符/UNC/ADS；
4. `resolveExisting` 规范化（符号链接、Windows 8.3 短名）；
5. `IsDenied` 检查全局私有路径；
6. 遍历 `s.roots`（按路径长度降序），`filepath.Rel` 做组件级包含判断；
7. 返回 `Resolved{Entry, RelSlash, RelOS, Abs}`，后续通过 `os.Root` 打开。

断点条件：

- `requestPath == "../outside"`：应返回 `relative path escapes the workspace`。
- `requestPath` 包含 `.termio`：应返回 `ErrDeniedPath`。
- `requestPath` 指向另一个工作区：应返回 `ErrOutsideRoots`（除非在 extra roots）。

---

## 6. 进度、超时、取消

### 6.1 进度

```
工具调用 progress(...)
  → dispatcher.progressSender.send
      → 250ms 节流
      → protocol.NewNotification("notifications/progress", {
            progressToken, progress, total, message,
            _meta: {CallID, WorkspaceID, SessionID},
        })
  → wsx.Writer.WriteJSON
  → 网关 deviceConn.readLoop
      → MethodProgress → debug 日志（v1 不转发 HTTP）
```

断点：

- `progressSender.send`：`p.workspaceID`、`p.sessionID`、距上次发送的时间。
- `session.go:readLoop` 的 progress 分支：`progress.Meta` 是否带对工作区。

手工触发：`search_files` 搜索一个很大的目录（>512 个条目），网关 debug 日志应出现 `tool progress`。

### 6.2 地端超时

```
dispatcher.handleCall
  → timer = AfterFunc(timeout, cancel(errCallTimeout))
  → 工具 ctx.Done
  → context.Cause == errCallTimeout
  → result = isError("tool timed out after ...")
  → cachePut（超时结果也缓存）
  → 网关 HTTP 200 + isError
```

断点：

- `dispatcher.go:handleCall` 的 `timer` 创建处：`timeout`。
- `cause` 判断处：应为 `errCallTimeout`。
- 工具内 `<-ctx.Done()` 是否真的被监听（工具实现负责）。

### 6.3 取消

```
云端 HTTP 断开 / 网关 ctx 到期
  → gateway.handleCall: ctx.Err() != nil
  → dc.notifyCancel(rpcID, callID, workspaceID, sessionID, "client_cancelled")
  → WS notifications/cancelled
  → daemon.readLoop: case MethodCancelled
  → dispatcher.cancelCall(params)
      → requestId → rpcToCall[rpcID] → key
      → 回退：callKey(workspaceID or default, sessionID, callID)
      → state.cancel(errCallCancelled)
  → 工具 ctx.Done
  → handleCall: cause == errCallCancelled → -32800
```

断点：

| 文件 / 函数 | 条件 | 观察 |
|---|---|---|
| `gateway/session.go:notifyCancel` | `callID == "debug-1"` | `workspaceID`/`sessionID` 是否随 cancel 下发 |
| `daemon/daemon.go:readLoop` | `msg.Method == "notifications/cancelled"` | `params.RequestID` |
| `daemon/dispatcher.go:cancelCall` | 同上 | `key`、`state` 是否为 nil |
| `daemon/dispatcher.go:handleCall` | `errors.Is(cause, errCallCancelled)` | 返回 `-32800` |

注意：取消只影响该调用；同工作区其他会话、其他工作区的调用不受影响。

---

## 7. 响应链路：daemon → 网关 → HTTP

```
dispatcher.handleCall
  → result.Meta.CallID/WorkspaceID/SessionID
  → protocol.NewResult(msg.ID, result)
  → d.send(parent, resp)
  → wsx.Writer.WriteJSON
  → gateway deviceConn.readLoop: msg.ID != "" → deliver(msg)
  → pending[msg.ID] 收到 rpcResult
  → deviceConn.request 返回
  → gateway.handleCall: resp.Error / resp.Result
  → HTTP 200 + CallToolResult
  → termioctl 打印 / 云端 Agent 读取
```

断点：

| 文件 / 函数 | 观察 |
|---|---|
| `daemon/dispatcher.go:handleCall` 的 `d.send` | `resp.Result` 中的 `_meta` |
| `gateway/session.go:deliver` | `msg.ID` 是否匹配 pending 键 |
| `gateway/gateway.go:handleCall` 的最后写响应处 | 状态码、`resp.Error` |

成功的响应应形如：

```json
{
  "content": [{"type": "text", "text": "..."}],
  "structuredContent": {...},
  "_meta": {
    "termio/callId": "debug-1",
    "termio/workspaceId": "ws_docs",
    "termio/sessionId": "chat-1"
  }
}
```

如果 `_meta` 缺工作区/会话，检查：

- 网关是否把 `workspace_id`/`session_id` 放进了 `CallMeta`；
- daemon 是否在 `NewResult` 前设置了 `result.Meta`。

---

## 8. 断线、重连与关闭

### 8.1 心跳

- 网关每 20s `Ping`；失败 → `dc.close(...)` → pending 全部 `errDeviceClosed` → 正在等待的 HTTP 调用 502。
- daemon 每 20s `Ping`；失败 → `cancel()` + `conn.CloseNow()` → `readLoop` 返回 → `Run` 进入退避。

断点：`daemon.go:heartbeat`、`session.go:heartbeat`。

### 8.2 重连

```
daemon.Run
  → runOnce 返回 (registered, err)
  → backoff 1s → 30s（±20% jitter）
  → 4003（被新连接取代）→ 冷却 5min
  → 注册成功后 backoff.reset()
```

断点：

- `daemon.go:Run` 的 `delay`：观察退避。
- `daemon.go:runOnce` 的 `supersededError` 分支。
- 网关 `hub.add`：旧连接是否被关闭。

### 8.3 关闭

- daemon 进程收到 SIGINT/SIGTERM → `signal.NotifyContext` 取消 → `sessionCtx` 取消 → `readLoop` 退出 → `disp.shutdown()` 取消所有 in-flight → `workspaces.Close()` 关闭所有 `os.Root`。
- 在途调用会得到 `errShutdown`，不写响应（连接正在关闭）。

断点：`dispatcher.shutdown`、`daemon.LoadWorkspaces` 返回后的 `defer workspaces.Close()`。

---

## 9. 多工作区 / 多会话测试矩阵

先按第 1 节启动两个工作区。每项都给出**输入、预期、主要断点**。

### T1 启动加载

- 输入：`termio run --config .debug/config.json`。
- 预期：日志 `workspaces=2`；`workspace list` 两行。
- 断点：`LoadWorkspaces` 的循环、`NewWorkspaceSet`。

### T2 注册上报

- 输入：daemon 连接网关。
- 预期：`GET /v1/devices` 中 `workspaces` 两项、`default_workspace_id=ws_default`。
- 断点：`daemon.register`、`gateway.handleWS`、`normalizeWorkspaceRegistration`。

### T3 默认工作区

- 输入：

```json
{"name":"system_info","arguments":{}}
```

- 预期：结果 `workspace` 是仓库根（`ws_default`）；`_meta.termio/workspaceId=ws_default`。
- 断点：`handleCall` 中 `workspaceID` 从空被补成 `ws_default`；`WorkspaceSet.Get("")`。

### T4 显式工作区

- 输入：

```json
{"name":"system_info","arguments":{},"workspace_id":"ws_docs","session_id":"chat-1"}
```

- 预期：`workspace` 是 `.debug/ws-docs`；`_meta` 回传 `ws_docs`/`chat-1`。
- 断点：`gw.handleCall` 的 `workspaceID`；`dispatcher.handleCall` 的 `ws.Sandbox.Workspace()`。

### T5 文件隔离

- 输入 1：

```json
{"name":"read_file","arguments":{"path":"notes.md"},"workspace_id":"ws_docs"}
```

→ 成功，内容 `hello from docs`。

- 输入 2：

```json
{"name":"read_file","arguments":{"path":"notes.md"},"workspace_id":"ws_default"}
```

→ isError（文件不存在），不会读到 docs 的文件。

- 断点：`read_file.Call` 的 `resolved.Abs`；`sandbox.Resolve` 的 `entry.display`。

### T6 未知工作区

- 输入：

```json
{"name":"system_info","arguments":{},"workspace_id":"ws_missing"}
```

- 预期：HTTP 400，body `{"error":{"code":-32012,...}}`。
- 断点：`gateway.handleCall` 的 `dc.hasWorkspace` 检查（HTTP 路径在这一步就拒绝，不会转发）；如果绕过网关直接发 WS，则断在 `WorkspaceSet.Get` 返回 `ErrWorkspaceNotFound` 和 `dispatcher.handleCall` 的 `-32012` 分支。

### T7 一个工作区多个会话

- 输入（同一 REPL）：

```json
{"name":"search_files","arguments":{"pattern":"*.md"},"workspace_id":"ws_docs","session_id":"chat-1","call_id":"same-1"}
{"name":"search_files","arguments":{"pattern":"*.md"},"workspace_id":"ws_docs","session_id":"chat-2","call_id":"same-1"}
```

- 预期：两次都执行（因为 session 不同），结果一致；`_meta.sessionId` 分别为 chat-1/chat-2。
- 再发一次 `chat-1` 的同键请求 → 命中缓存，不重新执行。
- 断点：`callKey`、`lookupCache`、`reserve`。

### T8 不同工作区并发

- 自动化测试（推荐）：

```bash
go test -count=1 -run TestE2EWorkspacesExecuteConcurrently -v ./internal/gateway
```

该测试给两个工作区各配一个阻塞工具，且每个工作区并发额度为 1；两个调用能同时进入工具，证明信号量是按工作区隔离的。

- 手工观察：在两个工作区分别跑 `search_files` 大目录，观察日志中的 `workspace_id` 交错出现；也可以用调试器在 `ws.Registry.Call` 处暂停 A，确认 B 的 HTTP 请求仍能推进（如果暂停了整个 daemon 线程则不行；Delve 暂停所有 goroutine，需用日志而非断点验证并发）。
- 断点：`handleCall` 中 `ws.Sem` 的写入；`cap(ws.Sem)`。

### T9 幂等跨工作区独立

- 输入：

```json
{"name":"system_info","arguments":{},"workspace_id":"ws_default","session_id":"s1","call_id":"same-2"}
{"name":"system_info","arguments":{},"workspace_id":"ws_docs","session_id":"s1","call_id":"same-2"}
```

- 预期：两次结果的工作区不同；第二次没有重放第一次。
- 断点：`lookupCache` 的 `key` 不同；`cachePut` 写入两个 key。

### T10 取消只影响一个工作区

- 用 Postman 假设备或真实 daemon 发两个慢调用（不同工作区），取消其中一个 HTTP 请求。
- 预期：只有对应 `callID` 收到 cancel；另一个继续执行/返回。
- 断点：`notifyCancel` 的参数、`cancelCall` 的 `key`。

### T11 工作区路径不泄露

- 输入：

```bash
curl -H "Authorization: Bearer dev-token" \
  http://127.0.0.1:8787/v1/devices/dev_debug/workspaces
```

- 预期：只有 `id`/`name`；没有 `path`、`extra_roots`、盘符。
- 断点：`WorkspaceSet.List`、`deviceSnapshot`。

### T12 新增/删除工作区需要重启

1. `termio workspace add --id ws_new ...`。
2. 不重启，调用 `workspace_id=ws_new` → 400 `-32012`。
3. 重启 daemon，再调用 → 成功。
4. `workspace remove ws_new`，不重启仍可调用（daemon 内存里还有）；重启后 → `-32012`。

### T13 默认工作区变更

1. `termio workspace default ws_docs`（重启 daemon）。
2. 不传 `workspace_id` 调用 → 落在 `ws_docs`。
3. 显式传 `workspace_id=ws_default` → 仍落在 `ws_default`。

---

## 10. 断点速查表

按“收到消息后的时间顺序”排列：

| # | 位置 | 触发时机 | 关键变量 |
|---|---|---|---|
| 1 | `gateway.handleCall` | HTTP 请求进入 | `body`、`deviceID`、`callID`、补默认后的 `workspaceID` |
| 2 | `gateway/session.go:request` | 写 WS 前 | `msg.ID`、`d.pending` |
| 3 | `daemon/readLoop` | 读到 tools/call | `data`、`msg.Method`、`msg.Params` |
| 4 | `daemon/dispatcher.handleCall` | 调度开始 | `params.Meta`、`key`、`fingerprint` |
| 5 | `daemon/workspace.go:Get` | 工作区解析 | `id`、`s.byID` |
| 6 | `dispatcher.lookupCache` / `reserve` | 幂等检查 | `completed`、`inflight`、`rpcToCall` |
| 7 | `dispatcher.handleCall` 信号量 | 并发控制 | `len(ws.Sem)`、`cap(ws.Sem)` |
| 8 | `tools.Registry.Call` | 进入工具 | `name`、registry 地址 |
| 9 | `sandbox.Resolve` | 路径解析 | `requestPath`、`canonical`、`entry.display` |
| 10 | `search_files.Call` / `read_file.Call` / `system_info.Call` | 工具执行 | 参数、`sandbox.Workspace()` |
| 11 | `progressSender.send` | 进度 | `p.workspaceID`、`progress` |
| 12 | `dispatcher.handleCall` 的 `cause` | 超时/取消 | `errCallTimeout` / `errCallCancelled` |
| 13 | `dispatcher.handleCall` 的 `d.send` | 回响应 | `result.Meta`、`isError` |
| 14 | `gateway/session.go:deliver` | 配对响应 | `msg.ID`、pending |
| 15 | `gateway.handleCall` 写 HTTP | 返回云端 | `status`、`resp.Error` |

Delve/VS Code 条件断点写法示例：

```text
params.Name == "system_info"
params.Meta.WorkspaceID == "ws_docs"
callID == "debug-1"
workspaceID == "ws_docs"
```

注意：

- 断点条件里的变量名以**当前栈帧**为准；不同函数里变量名不同。
- Delve 暂停会暂停整个进程（所有 goroutine），不要用断点验证“并发是否真的同时执行”；用日志、测试或 `ws.Sem` 状态验证。
- 不要在 Watch/日志里输出 token；`config.Config.Token` 只在 `loadConfig` 阶段可见，调试完清除。

---

## 11. 自动化测试与日志

### 11.1 跑的测试

```bash
go test -count=1 ./...
go vet ./...
gofmt -l .        # 必须为空
```

重点端到端测试：

```bash
go test -count=1 -v ./internal/gateway
```

| 测试 | 覆盖 |
|---|---|
| `TestE2ESearchReadAndSystemInfo` | 基本 HTTP → WS → 工具 → HTTP |
| `TestE2EWorkspaceRoutingAndIsolation` | 默认/显式工作区、结果 meta、跨工作区读失败、未知工作区 |
| `TestE2ECallIDScopedByWorkspaceAndSession` | `(workspace, session, callId)` 幂等域 |
| `TestE2EWorkspacesExecuteConcurrently` | 每工作区独立并发额度 |
| `TestE2EWorkspacesEndpoint` | 工作区元数据、不泄露路径 |
| `TestE2EDuplicateCallIDReplaysCache` | 单工作区重放 |
| `TestE2EToolTimeoutIsResult` | 地端超时是 isError |
| `TestE2EClientDisconnectCancelsTool` | 客户端断开 → cancel |
| `TestE2EReconnectAfterSessionClosed` | 断线重连 |
| `TestE2EFakeDeviceManualProtocol` | 手工协议帧、默认工作区注入 |

配置/工作区单测：

```bash
go test -count=1 -v ./internal/config ./internal/daemon
```

### 11.2 日志观察点

daemon（`.debug/termio.log`，`--log-level debug`）：

- `termio starting`：工作区数量、默认工作区、工具数。
- `workspace loaded`：每个工作区的 ID/name/path/roots。
- `registered with gateway`：注册结果。
- `connection ended` / `retry_in`：重连。
- `call abandoned during shutdown`：关闭时在途调用。

网关（`--log-level debug`）：

- `device connected`：`workspaces`、`default_workspace`。
- `tool call`：`workspace_id`、`session_id`、`call_id`、状态码、耗时。
- `tool progress`：仅 debug。
- `device disconnected`。

### 11.3 建议的日志增强（仅调试时）

- 在 `dispatcher.handleCall` 进入处临时加：`workspace_id`、`session_id`、`call_id`、`key`。
- 在 `sandbox.Resolve` 返回处临时加：`canonical`、`entry.display`。
- 不要记录完整 arguments（可能含敏感路径/内容）；提交前删除。

---

## 12. 常见问题定位

| 现象 | 优先检查 |
|---|---|
| 调用总是落在默认工作区 | HTTP body 是否用了 `workspace_id`（不是 `workspaceId`）；网关 `handleCall` 的 `body.WorkspaceID` |
| `-32012 unknown workspace` | `workspace list`；daemon 是否重启；网关是否收到新的注册；大小写/拼写 |
| 结果 `_meta.workspaceId` 为空 | daemon 是否设置了 `result.Meta`；网关是否传了 `WorkspaceID` |
| 同一工作区两个会话互相重放 | `callKey` 是否包含 session；客户端是否传了不同的 `session_id` |
| 不同工作区串文件 | `WorkspaceSet.Get` 返回是否同一个 `*Workspace`；`sandbox.Resolve` 的 `entry.display`；是否误用了默认参数 |
| 两个工作区不能并发 | `cap(ws.Sem)` 是否被全局共享；`handleCall` 是否用了旧的全局 `sem` |
| 并发满返回 `workspace is busy` | 该工作区在途调用达到 `max_concurrent_calls`；调大配置或等待 |
| 路径越界 | `sandbox.Resolve` 的 `canonical` 与 `entry.display`；相对路径是否以工作区为基准 |
| `.termio` 仍可读 | deny 列表是否包含 `AppDataDir`；`IsDenied` 是否被调用 |
| 修改工作区后不生效 | 新增/删除/默认变更是否重启 daemon；是否用了 `Termio Daemon (no init)` |
| VS Code 配置被重置 | 组合调试或 `Termio Daemon` 会执行 `prepare-debug`；改用 `no init` 保留手改配置 |
| 对端收不到取消 | `notifyCancel` 是否带 `requestId`；daemon `cancelCall` 的 `rpcToCall` 是否有该 id |

---

## 13. 一页流程总结

```
[启动]
  gateway.New → 路由
  termio run → loadConfig → LoadWorkspaces → 每个工作区 opens os.Root
             → daemon.New → Run → dial → register(workspaces) → readLoop

[调用]
  HTTP POST
    → gateway.handleCall
        auth → decode → 补默认 workspace → 组 _meta → dc.request
    → WebSocket tools/call
    → daemon.readLoop
        → go dispatcher.handleCall
            decode → Get(workspace) → key(work,sess,call) → 缓存/信号量/超时
            → ws.Registry.Call
                → sandbox.Resolve → os.Root → tool
            → result.Meta(work,sess,call) → cachePut → WS response
    → gateway.deliver → HTTP 200 CallToolResult

[异常]
  超时 → daemon isError 结果
  取消 → notifications/cancelled → ctx cancel → -32800
  断开 → pending 失败 → 502；daemon 退避重连
  未知工作区 → -32012 / HTTP 400
```

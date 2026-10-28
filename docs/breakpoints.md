# 全链路断点手册（VS Code + Delve）

> 目标：从进程启动到一条工具调用返回，**每一段数据流都能在调试器里看到**。
>
> 基准代码：提交 `956ab24` 之后的当前 main。行号会随代码变动；函数名和条件不会。行号漂移时用“函数名 + 条件文本”在文件里搜索，或使用函数断点。
>
> 先按 [debug-full-chain.md](debug-full-chain.md) 的第 1 节把环境跑起来：F5 会自动准备 `ws_default` + `ws_docs` 两个工作区并生成 `notes.md`。

---

## 0. VS Code 里怎么下断点

### 0.1 普通断点

1. 打开 Go 文件。
2. 点击行号左侧空白处，出现红点；或光标在该行按 `F9`。
3. `F5` 启动 **Termio Daemon** 或 **Termio: Gateway + Daemon**。
4. 命中后，左侧 **Run and Debug** 面板看：
   - **Variables**：Locals / Globals；
   - **Watch**：手动加表达式；
   - **Call Stack**：当前 goroutine 的调用栈；
   - **Debug Console**：执行 Delve 命令。

### 0.2 条件断点（最常用）

右键红点 → **Edit Breakpoint** → **Expression**，输入 Go 表达式。只有表达式为 true 才停。

示例：

```text
workspaceID == "ws_docs"
callID == "dbg-1"
sessionID == "chat-1"
params.Name == "search_files"
msg.Method == "tools/call"
body.WorkspaceID == ""
len(ws.Sem) > 0
```

注意：

- 表达式在**当前栈帧**求值；不同函数里变量名不同。
- 字符串要用双引号。
- 不确定变量名时，先在函数入口打普通断点，看 Variables 面板。

### 0.3 日志断点（Logpoint，不停下来）

右键行号左侧 → **Add Logpoint**，消息写：

```text
gateway workspace={workspaceID} call={callID} session={sessionID}
```

或：

```text
daemon key={key} ws={workspaceID} call={callID}
```

Logpoint 不会暂停进程，适合观察并发和进度。若当前 Go 扩展版本对 `{}` 插值不生效，改用条件断点（条件写成 `false` 不会停）或在 Debug Console 手动 `print`。

### 0.4 函数断点

Run and Debug 面板 → **Breakpoints** → `+` → 输入函数名。Go/Delve 常见写法：

```text
termio/internal/daemon.(*dispatcher).handleCall
termio/internal/gateway.(*Server).handleCall
termio/internal/tools.(*Sandbox).Resolve
```

不同 VS Code Go 扩展版本对函数断点语法支持不一致；解析失败就直接用“文件 + 行号”。

### 0.5 Debug Console 常用命令

断住后，在 **Debug Console** 输入：

```text
print workspaceID
print key
print string(msg.Params)
print string(msg.Result)
locals
args
goroutines
goroutine 12
stack
up
down
```

- `goroutines`：所有 goroutine，找到 HTTP/WS/工具执行对应的那个。
- `goroutine <id>`：切换当前 goroutine。
- `up` / `down`：在调用栈里移动。
- `print` 可以看 map/slice 内部；大对象建议先 `print len(x)`。

### 0.6 调试多个 goroutine

Delve 命中断点时默认暂停**整个进程**。HTTP 请求、心跳、其他工具调用都会停。想观察并发：

- 用 Logpoint 而不是普通断点；
- 或只在一个 goroutine 条件断点（如 `workspaceID == "ws_docs"`）；
- 或在 Debug Console 用 `goroutines` 查看所有 goroutine 的栈，确认每条链路都在。

### 0.7 断点没命中怎么办

1. 确认改的是正在调试的代码：VS Code 启动的是 `go run`/Delve 编译的版本，不是 `bin/*.exe`。
2. 确认请求真的到了这个进程：看网关/daemon 的 debug 日志。
3. 条件断点表达式写错会静默不停；先删条件试普通断点。
4. 分支可能没被触发：对照本文第 11、12 节的“触发方式”。
5. 请求被网关提前拒绝：HTTP 400/401/404 不会进入 daemon；先断网关。
6. 断点打在了异步 goroutine 里：`go disp.handleCall` 会在新 goroutine 执行，Call Stack 可能显示多个 goroutine，切换过去。

---

## 1. 启动阶段断点

### 1.1 网关启动

| 位置 | 函数 | 断点/条件 | 看到什么 |
|---|---|---|---|
| `cmd/termio-gateway/main.go:69` | `run` | 普通 | `gateway.Options`：token、devices、心跳、超时 |
| `internal/gateway/gateway.go:53` | `New` | 普通 | `opts`；token 校验；心跳夹到 5–60s；超时上限 5m |
| `internal/gateway/gateway.go:82` | `New` | 普通 | `mux` 路由是否注册了 6 个端点 |
| `internal/gateway/gateway.go:94` | `Handler` | 普通 | 返回的 handler |
| `cmd/termio-gateway/main.go:89` | `run` | 普通 | `net.Listen` 成功后的 `ln.Addr()` |
| `cmd/termio-gateway/main.go:93` | `run` | 普通 | `gateway listening` 日志参数 |

### 1.2 daemon 启动

| 位置 | 函数 | 断点/条件 | 看到什么 |
|---|---|---|---|
| `cmd/termio/main.go:186` | `runDaemon` | 普通 | 启动入口 |
| `cmd/termio/main.go:195` | `runDaemon` | 普通 | `cfg`、`path` |
| `internal/config/config.go:178` | `Load` | 普通 | 文件路径、解析错误 |
| `internal/config/config.go:209` | `normalizeWorkspaces` | 普通 | 旧字段迁移、路径规范化、重复路径 |
| `internal/config/config.go:481` | `Validate` | 普通 | 每个工作区是否存在、默认 ID 是否合法 |
| `internal/daemon/workspace.go:68` | `LoadWorkspaces` | 普通 | `cfg.Workspaces`、deny 列表 |
| `internal/daemon/workspace.go:97` | `LoadWorkspaces` | 普通 | `tools.NewSandbox` 的返回值 |
| `internal/tools/sandbox.go:48` | `NewSandbox` | 普通 | `workspace`、`extraRoots`、`deny` |
| `internal/tools/sandbox.go:89` | `NewSandbox` | 普通 | `os.OpenRoot` 打开的每个 root |
| `internal/daemon/workspace.go:102` | `LoadWorkspaces` | 普通 | `Workspace{ID, Name, Sandbox, Registry, Sem}` |
| `internal/daemon/workspace.go:110` | `LoadWorkspaces` | 普通 | `NewWorkspaceSet` 的结果 |
| `cmd/termio/main.go:205` | `runDaemon` | 普通 | `workspaces` 是否非空 |
| `cmd/termio/main.go:215` | `runDaemon` | 普通 | `workspaces` 数量、默认工作区、工具数 |
| `cmd/termio/main.go:227` | `runDaemon` | 普通 | 每个工作区的 ID/name/path/roots |
| `internal/daemon/daemon.go:47` | `New` | 普通 | `cfg`、`WorkspaceSet` 指针 |
| `internal/daemon/daemon.go:56` | `Run` | 普通 | 重连循环入口 |
| `internal/daemon/daemon.go:95` | `runOnce` | 普通 | 每次连接尝试 |

要看的变量：

- `cfg.Workspaces`：两个工作区的路径、额外根。
- `sb.Roots()`：工作区在前，额外根在后。
- `cap(ws.Sem)`：每工作区并发额度。
- `workspaces.List()`：只有 ID/name，没有 path。

### 1.3 注册

| 位置 | 函数 | 断点/条件 | 看到什么 |
|---|---|---|---|
| `internal/daemon/daemon.go:104` | `runOnce` | 普通 | `conn`、`resp`、dial 错误 |
| `internal/daemon/daemon.go:143` | `register` | 普通 | 注册入口 |
| `internal/daemon/daemon.go:145` | `register` | 普通 | `RegisterParams` |
| `internal/daemon/daemon.go:155` | `register` | 普通 | `Tools` 并集 |
| `internal/daemon/daemon.go:156` | `register` | 普通 | `Workspaces`：`ws_default`、`ws_docs` |
| `internal/daemon/daemon.go:157` | `register` | 普通 | `DefaultWorkspaceID` |
| `internal/daemon/daemon.go:158` | `register` | 普通 | `MaxInflight` |
| `internal/daemon/daemon.go:165` | `register` | 普通 | 写出的注册帧 |
| `internal/daemon/daemon.go:172` | `register` | 普通 | 读到的注册响应 |
| `internal/daemon/daemon.go:210` | `register` | 普通 | `registered with gateway` 日志 |
| `internal/gateway/gateway.go:341` | `handleWS` | 普通 | `deviceID`、请求头 |
| `internal/gateway/gateway.go:355` | `handleWS` | 普通 | `websocket.Accept` 结果 |
| `internal/gateway/gateway.go:365` | `handleWS` | 普通 | 注册原始帧 `data` |
| `internal/gateway/gateway.go:376` | `handleWS` | 普通 | `msg` 解析结果 |
| `internal/gateway/gateway.go:385` | `handleWS` | 普通 | `params` |
| `internal/gateway/gateway.go:496` | `normalizeWorkspaceRegistration` | 普通 | 工作区 ID/名称/默认 ID |
| `internal/gateway/gateway.go:409` | `handleWS` | 普通 | `maxInflight` 夹紧后的值 |
| `internal/gateway/gateway.go:435` | `handleWS` | 普通 | `newDeviceConn` 参数 |
| `internal/gateway/gateway.go:436` | `handleWS` | 普通 | `hub.add`；旧连接是否被替换 |
| `internal/gateway/session.go:34` | `hub.add` | 普通 | `h.devices`、old 连接 |
| `internal/gateway/gateway.go:448` | `handleWS` | 普通 | 心跳 goroutine 启动 |
| `internal/gateway/gateway.go:449` | `handleWS` | 普通 | 进入设备读循环 |
| `internal/gateway/gateway.go:450` | `handleWS` | 普通 | 连接结束原因 |

---

## 2. HTTP 入站断点

| 位置 | 函数 | 断点/条件 | 看到什么 |
|---|---|---|---|
| `internal/gateway/gateway.go:192` | `handleCall` | 普通 | HTTP 入口 |
| `internal/gateway/gateway.go:194` | `handleCall` | `r.URL.Path` 含 `tools/call` | `deviceID`、token |
| `internal/gateway/gateway.go:454` | `tokenMatchesDevice` | 普通 | `deviceID`、`token`、匹配结果 |
| `internal/gateway/gateway.go:203` | `handleCall` | 普通 | `body`；解码错误在此返回 |
| `internal/gateway/gateway.go:207` | `handleCall` | `body.Name == ""` | 空工具名 |
| `internal/gateway/gateway.go:211` | `handleCall` | 普通 | `callID`（空则生成） |
| `internal/gateway/gateway.go:215` | `handleCall` | 普通 | `validCallID` 失败 |
| `internal/gateway/gateway.go:220` | `handleCall` | 普通 | `dc` 是否存在（设备离线） |
| `internal/gateway/gateway.go:226` | `handleCall` | 普通 | `body.WorkspaceID` |
| `internal/gateway/gateway.go:228` | `handleCall` | `body.WorkspaceID == ""` | 注入的默认工作区 ID |
| `internal/gateway/gateway.go:231` | `handleCall` | 普通 | workspace ID 格式校验 |
| `internal/gateway/gateway.go:237` | `handleCall` | 普通 | `dc.hasWorkspace`（注册列表校验） |
| `internal/gateway/gateway.go:243` | `handleCall` | 普通 | `sessionID` 校验 |
| `internal/gateway/gateway.go:249` | `handleCall` | `body.TimeoutMS > 0` | 客户端超时 |
| `internal/gateway/gateway.go:258` | `handleCall` | 普通 | 最终 `timeout` |
| `internal/gateway/gateway.go:263` | `handleCall` | 普通 | 空 arguments 补 `{}` |
| `internal/gateway/gateway.go:267` | `handleCall` | 普通 | 即将发给 daemon 的 `ToolCallParams` |
| `internal/gateway/gateway.go:285` | `handleCall` | 普通 | `ctx` 的 deadline（timeout+2s） |
| `internal/gateway/gateway.go:289` | `handleCall` | 普通 | 调用 `dc.request` 的时机 |
| `internal/gateway/session.go:177` | `request` | 普通 | `msg.ID`、`msg.Method`、`string(msg.Params)` |
| `internal/gateway/session.go:187` | `request` | 普通 | `d.pending` 插入 |
| `internal/gateway/session.go:198` | `request` | 普通 | 写 WebSocket 的帧 |
| `internal/gateway/session.go:202` | `request` | 普通 | 等待 select |

**这一段的完整数据流：**

```
HTTP body
  → callRequest（DisallowUnknownFields）
  → workspace 默认值
  → ToolCallParams{Name, Arguments, Meta{CallID, WorkspaceID, SessionID, TimeoutMS}}
  → protocol.NewRequest（JSON-RPC id=g_...）
  → deviceConn.request
  → d.pending[g_id]
  → wsx.WriteJSON（WebSocket 文本帧）
```

---

## 3. daemon 收帧与调度断点

| 位置 | 函数 | 断点/条件 | 看到什么 |
|---|---|---|---|
| `internal/daemon/daemon.go:241` | `readLoop` | 普通 | 每次 `conn.Read` |
| `internal/daemon/daemon.go:249` | `readLoop` | 普通 | `msg` 解析结果 |
| `internal/daemon/daemon.go:263` | `readLoop` | `msg.Method == "tools/call"` | 进入工具调用分支 |
| `internal/daemon/daemon.go:266` | `readLoop` | `msg.Method == "tools/list"` | 进入工具列表分支（Postman Refresh tools 触发） |
| `internal/daemon/daemon.go:267` | `readLoop` | `msg.Method == "notifications/cancelled"` | 取消分支 |
| `internal/daemon/daemon.go:274` | `readLoop` | `msg.Method != "" && msg.ID != ""` | 未知方法分支 |
| `internal/daemon/dispatcher.go:84` | `handleCall` | 普通 | 调度入口 |
| `internal/daemon/dispatcher.go:86` | `handleCall` | 普通 | `DecodeParams` 错误 |
| `internal/daemon/dispatcher.go:90` | `handleCall` | `params.Name == ""` | 空工具名 |
| `internal/daemon/dispatcher.go:95` | `handleCall` | `callID == ""` | 缺 callId |
| `internal/daemon/dispatcher.go:99` | `handleCall` | 普通 | callId 格式错误 |
| `internal/daemon/dispatcher.go:103` | `handleCall` | 普通 | `workspaceID`、`sessionID` |
| `internal/daemon/dispatcher.go:105` | `handleCall` | 普通 | session 格式错误 |
| `internal/daemon/dispatcher.go:109` | `handleCall` | 普通 | `WorkspaceSet.Get` |
| `internal/daemon/workspace.go:122` | `Get` | `id == ""` | 默认工作区替换 |
| `internal/daemon/workspace.go:127` | `Get` | 普通 | 未知工作区错误 |
| `internal/daemon/dispatcher.go:118` | `handleCall` | 普通 | 归一化后的 `workspaceID` |
| `internal/daemon/dispatcher.go:119` | `handleCall` | 普通 | `key = workspace\0session\0call` |
| `internal/daemon/dispatcher.go:123` | `handleCall` | 普通 | 缓存快路径 |
| `internal/daemon/dispatcher.go:133` | `handleCall` | 普通 | 抢到工作区信号量 |
| `internal/daemon/dispatcher.go:136` | `handleCall` | 普通 | 信号量满（-32010） |
| `internal/daemon/dispatcher.go:140` | `handleCall` | 普通 | `timeout` 计算 |
| `internal/daemon/dispatcher.go:144` | `handleCall` | 普通 | `reserve` 结果 |
| `internal/daemon/dispatcher.go:162` | `handleCall` | 普通 | 超时定时器 |
| `internal/daemon/dispatcher.go:192` | `handleCall` | 普通 | `ws.Registry.Call` |
| `internal/daemon/dispatcher.go:195` | `handleCall` | 普通 | `context.Cause` |
| `internal/daemon/dispatcher.go:198` | `handleCall` | `errors.Is(cause, errShutdown)` | 关闭时在途调用 |
| `internal/daemon/dispatcher.go:201` | `handleCall` | `errors.Is(cause, errCallCancelled)` | 取消 |
| `internal/daemon/dispatcher.go:204` | `handleCall` | `errors.Is(cause, errCallTimeout)` | 超时 |
| `internal/daemon/dispatcher.go:207` | `handleCall` | `parent.Err() != nil` | 连接/session 关闭 |
| `internal/daemon/dispatcher.go:213` | `handleCall` | `callErr != nil` | 工具错误 |
| `internal/daemon/dispatcher.go:216` | `handleCall` | 普通 | ArgError / UnknownToolError |
| `internal/daemon/dispatcher.go:223` | `handleCall` | 普通 | 结果 `_meta` 赋值 |
| `internal/daemon/dispatcher.go:226` | `handleCall` | 普通 | `NewResult` |
| `internal/daemon/dispatcher.go:231` | `handleCall` | 普通 | `cachePut` |
| `internal/daemon/dispatcher.go:232` | `handleCall` | 普通 | 最终响应发出 |

**工作区路由的关键变量：**

```text
params.Meta.WorkspaceID   // 客户端原始值，可能为空
workspaceID               // Get 之后归一化，空值已变默认
ws.ID                     // 实际工作区
ws.Sandbox.Workspace()    // 实际目录
key                       // workspace + session + callId
fingerprint               // 工具名 + arguments
```

---

## 4. 幂等缓存/并发分支断点

| 位置 | 函数 | 断点/条件 | 触发方式 | 预期 |
|---|---|---|---|---|
| `internal/daemon/dispatcher.go:123` | `handleCall` | 普通 | 同一个 `call_id` + 同参数第二次 | 命中缓存，`d.send(replay)`，不进工具 |
| `internal/daemon/dispatcher.go:124` | `handleCall` | 普通 | 同一 callId + 改参数 | `-32602` |
| `internal/daemon/dispatcher.go:133` | `handleCall` | 普通 | 正常调用 | 进入信号量 |
| `internal/daemon/dispatcher.go:136` | `handleCall` | 普通 | `max_concurrent_calls=1`，同时两个慢调用 | 一个 `-32010 workspace is busy` |
| `internal/daemon/dispatcher.go:146` | `handleCall` | 普通 | 同 key 两个并发调用 | 第二个 `-32005` |
| `internal/daemon/dispatcher.go:149` | `handleCall` | 普通 | 并发竞态/缓存指纹冲突 | `-32602` |
| `internal/daemon/dispatcher.go:152` | `handleCall` | 普通 | 快路径和 reserve 之间的竞态 | 重放缓存 |
| `internal/daemon/dispatcher.go:265` | `lookupCache` | 普通 | 每次调用 | `completed`、`expires` |
| `internal/daemon/dispatcher.go:282` | `reserve` | 普通 | 每次未命中快路径的调用 | `inflight`、`rpcToCall` |
| `internal/daemon/dispatcher.go:317` | `cachePut` | 普通 | 调用成功/超时后 | 写入 `completed` |
| `internal/daemon/dispatcher.go:324` | `cachePut` | 普通 | 累计 1024 条后 | 淘汰过期/清空 |
| `internal/daemon/dispatcher.go:308` | `release` | 普通 | 调用结束 | 从 `inflight`/`rpcToCall` 移除 |

**同 key 并发怎么造：**

- 把 `max_concurrent_calls` 调到 1 以上；
- 找一个耗时工具/大目录，或用调试器在 `ws.Registry.Call` 前暂停（注意 Delve 会暂停整个进程，第二请求要等恢复才被读取，不适合稳定复现）；
- 更可靠：用自动化测试 `TestE2EWorkspacesExecuteConcurrently` 的阻塞工具思路，或在测试里调试 `reserve` 分支；
- 日常验证缓存命中：Postman **Duplicate call_id (run twice)**。

---

## 5. 工作区与沙箱断点

| 位置 | 函数 | 断点/条件 | 看到什么 |
|---|---|---|---|
| `internal/daemon/workspace.go:122` | `WorkspaceSet.Get` | 普通 | `id`、`byID`、`defaultID` |
| `internal/daemon/workspace.go:137` | `WorkspaceSet.List` | 普通 | 只含 ID/name 的列表 |
| `internal/daemon/workspace.go:149` | `WorkspaceSet.Specs` | 普通 | 工具并集、去重 |
| `internal/tools/sandbox.go:132` | `Sandbox.Resolve` | 普通 | `requestPath` |
| `internal/tools/sandbox.go:134` | `Resolve` | `requestPath == ""` | 默认到工作区 |
| `internal/tools/sandbox.go:137` | `Resolve` | 普通 | 相对/绝对路径分支 |
| `internal/tools/sandbox.go:140` | `Resolve` | 普通 | 相对 `..` 逃逸 |
| `internal/tools/sandbox.go:144` | `Resolve` | 普通 | 绝对路径格式校验（UNC/ADS/盘符） |
| `internal/tools/sandbox.go:150` | `Resolve` | 普通 | `canonical` 规范化结果 |
| `internal/tools/sandbox.go:154` | `Resolve` | 普通 | deny 命中（`.termio`） |
| `internal/tools/sandbox.go:158` | `Resolve` | 普通 | 与每个 root 的 `filepath.Rel` |
| `internal/tools/sandbox.go:162` | `Resolve` | 普通 | 跨根/越界 |
| `internal/tools/sandbox.go:176` | `Resolve` | 普通 | `ErrOutsideRoots` |
| `internal/tools/sandbox.go:182` | `IsDenied` | 普通 | 私有路径检查 |
| `internal/tools/sandbox.go:268` | `resolveExisting` | 普通 | 符号链接/短名/缺失后缀 |
| `internal/tools/sandbox.go:292` | `validateAbsolutePath` | 普通 | Windows UNC/设备/ADS |

**工作区隔离验证：**

```json
{"name":"read_file","arguments":{"path":"notes.md"},"workspace_id":"ws_docs"}
```

- 断在 `Resolve`：`requestPath="notes.md"`，`requested` 应变成 `.../.debug/ws-docs/notes.md`，`entry.display` 是 `ws-docs`。
- 换 `workspace_id=ws_default` 再发：`Resolve` 的 `entry.display` 变成仓库根，`open` 找不到文件 → isError。

---

## 6. 工具执行断点

| 位置 | 函数 | 断点/条件 | 看到什么 |
|---|---|---|---|
| `internal/daemon/dispatcher.go:192` | `handleCall` | 普通 | 进入 `ws.Registry.Call` |
| `internal/tools/registry.go:74` | `Registry.Call` | 普通 | `name`、registry 地址 |
| `internal/tools/registry.go:77` | `Registry.Call` | 普通 | 未知工具 |
| `internal/tools/registry.go:87` | `decodeArgs` | 普通 | 参数严格解析 |
| `internal/tools/search_files.go:75` | `SearchFiles.Call` | `args.Pattern == "*.md"` | 搜索参数 |
| `internal/tools/search_files.go:107` | `SearchFiles.Call` | 普通 | `path` 省略 → 工作区 |
| `internal/tools/search_files.go:110` | `SearchFiles.Call` | 普通 | `Sandbox.Resolve` |
| `internal/tools/search_files.go:132` | `SearchFiles.Call` | 普通 | `fs.WalkDir` |
| `internal/tools/search_files.go:134` | `SearchFiles.Call` | 普通 | 每次检查 ctx |
| `internal/tools/search_files.go:146` | `SearchFiles.Call` | 普通 | 跳过 `.termio` |
| `internal/tools/search_files.go:154` | `SearchFiles.Call` | 普通 | 每 512 项进度 |
| `internal/tools/search_files.go:168` | `SearchFiles.Call` | 普通 | 深度超限 |
| `internal/tools/search_files.go:177` | `SearchFiles.Call` | 普通 | symlink 跳过 |
| `internal/tools/search_files.go:180` | `SearchFiles.Call` | `!args.IncludeHidden` | 隐藏/依赖目录 |
| `internal/tools/search_files.go:192` | `SearchFiles.Call` | 普通 | 文件名匹配 |
| `internal/tools/search_files.go:208` | `SearchFiles.Call` | 普通 | 达到 max_results |
| `internal/tools/read_file.go:59` | `ReadFile.Call` | 普通 | 读取入口 |
| `internal/tools/read_file.go:78` | `ReadFile.Call` | 普通 | `Sandbox.Resolve` |
| `internal/tools/read_file.go:82` | `ReadFile.Call` | 普通 | `os.Root.Open` |
| `internal/tools/read_file.go:96` | `ReadFile.Call` | 普通 | ctx 取消检查 |
| `internal/tools/read_file.go:102` | `ReadFile.Call` | 普通 | `io.ReadFull` |
| `internal/tools/read_file.go:115` | `ReadFile.Call` | 普通 | UTF-8/二进制判定 |
| `internal/tools/read_file.go:138` | `ReadFile.Call` | 普通 | base64 分支 |
| `internal/tools/system_info.go:37` | `SystemInfo.Call` | 普通 | 返回当前工作区/roots |

**一个正常 `system_info` 调用应该依次命中：**

1. `gateway.handleCall`（192）
2. `gateway.session.request`（177）
3. `daemon.readLoop`（241/263）
4. `daemon.dispatcher.handleCall`（84）
5. `WorkspaceSet.Get`（122）
6. `callKey`（235）
7. `lookupCache`（265）
8. `ws.Sem`（133）
9. `reserve`（282）
10. `Registry.Call`（74）
11. `SystemInfo.Call`（37）
12. `result.Meta`（223）
13. `cachePut`（231）
14. `dispatcher.send`（395）
15. `gateway.session.deliver`（257）
16. `gateway.handleCall` 写响应（324–327）

如果这 16 个点里有任何一段没停，说明这条数据流没有按预期经过；先查上一段。

---

## 7. 进度断点

| 位置 | 函数 | 断点/条件 | 触发方式 | 看到什么 |
|---|---|---|---|---|
| `internal/daemon/dispatcher.go:432` | `progressSender.send` | 普通 | 大目录搜索（>512 项） | `progress`、`total`、`message` |
| `internal/daemon/dispatcher.go:438` | `progressSender.send` | 普通 | 高频进度 | 250ms 节流 |
| `internal/daemon/dispatcher.go:445` | `progressSender.send` | 普通 | 实际构造通知 | `_meta` 里的 call/workspace/session |
| `internal/daemon/dispatcher.go:459` | `progressSender.send` | 普通 | 写不进网关时 | 丢弃日志 |
| `internal/gateway/session.go:231` | `deviceConn.readLoop` | `msg.Method == "notifications/progress"` | 真实 daemon 进度 | `progress.Meta` |
| `internal/gateway/session.go:233` | `deviceConn.readLoop` | 普通 | 解码进度 | `progress` |

v1 网关只把进度写 debug 日志，不通过 HTTP 流式返回；看到 `tool progress` 日志就说明这段通了。

---

## 8. 超时断点

| 位置 | 函数 | 断点/条件 | 触发方式 | 预期 |
|---|---|---|---|---|
| `internal/daemon/dispatcher.go:140` | `handleCall` | 普通 | `timeout_ms` 很小 | 最终 timeout |
| `internal/daemon/dispatcher.go:162` | `handleCall` | 普通 | 慢工具 | 定时器触发 |
| `internal/daemon/dispatcher.go:195` | `handleCall` | 普通 | 超时后 | `context.Cause` |
| `internal/daemon/dispatcher.go:204` | `handleCall` | 普通 | 超时 | `errCallTimeout` |
| `internal/tools/search_files.go:134` | `SearchFiles.Call` | 普通 | 搜索中 ctx 到期 | `ctx.Done` |
| `internal/gateway/gateway.go:285` | `handleCall` | 普通 | `timeout+2s` | 网关 deadline |
| `internal/gateway/gateway.go:299` | `handleCall` | 普通 | daemon 不回应 | 504 |
| `internal/gateway/session.go:203` | `request` | 普通 | ctx 到期 | `ctx.Err()` |

**制造超时：**

- 找一个大目录（例如在 `.debug/ws-docs` 下创建几万个小文件），`search_files` + `timeout_ms: 1000`；
- 或在 daemon 的 `Registry.Call` 前暂停，等网关超过 `timeout+2s`（Delve 暂停期间工具不会推进，网关会 504）；
- 重置：恢复运行后，daemon 可能返回超时 isError 或收到 cancel，取决于暂停时长。

---

## 9. 取消断点

| 位置 | 函数 | 断点/条件 | 触发方式 | 预期 |
|---|---|---|---|---|
| `internal/gateway/gateway.go:292` | `handleCall` | `ctx.Err() != nil` | 客户端断开/网关超时 | 发送 cancel |
| `internal/gateway/gateway.go:293` | `handleCall` | 普通 | 同上 | `dc.notifyCancel(...)` |
| `internal/gateway/session.go:312` | `notifyCancel` | 普通 | 同上 | cancel 帧的 `_meta` |
| `internal/daemon/daemon.go:267` | `readLoop` | `msg.Method == "notifications/cancelled"` | 收到 cancel | `params.RequestID` |
| `internal/daemon/dispatcher.go:343` | `cancelCall` | 普通 | 同上 | `key`、`state` |
| `internal/daemon/dispatcher.go:346` | `cancelCall` | `params.RequestID != ""` | 网关路径 | `rpcToCall` 查找 |
| `internal/daemon/dispatcher.go:349` | `cancelCall` | 普通 | 回退路径 | 三元组 key |
| `internal/daemon/dispatcher.go:358` | `cancelCall` | 普通 | 找到调用 | `cancel(errCallCancelled)` |
| `internal/daemon/dispatcher.go:201` | `handleCall` | 普通 | 取消生效 | `-32800` |

**制造取消：**

```bash
curl --max-time 1 -X POST "http://127.0.0.1:8787/v1/devices/dev_debug/tools/call" \
  -H "Authorization: Bearer dev-token" -H "Content-Type: application/json" \
  -d '{"name":"search_files","arguments":{"pattern":"*","max_depth":64},"timeout_ms":30000}'
```

让搜索超过 1 秒；curl 断开后网关发送 cancel。

---

## 10. 响应回程断点

| 位置 | 函数 | 断点/条件 | 看到什么 |
|---|---|---|---|
| `internal/daemon/dispatcher.go:223` | `handleCall` | 普通 | `result.Meta` 赋值 |
| `internal/daemon/dispatcher.go:226` | `handleCall` | 普通 | `NewResult` |
| `internal/daemon/dispatcher.go:231` | `handleCall` | 普通 | 缓存结果 |
| `internal/daemon/dispatcher.go:232` | `handleCall` | 普通 | 发送响应 |
| `internal/daemon/dispatcher.go:395` | `send` | 普通 | `msg.ID`、`msg.Result` |
| `internal/wsx/writer.go:29` | `WriteJSON` | 普通 | 序列化后的帧 |
| `internal/gateway/session.go:212` | `deviceConn.readLoop` | 普通 | 从设备读到帧 |
| `internal/gateway/session.go:251` | `readLoop` | `msg.ID != ""` | 进入响应分支 |
| `internal/gateway/session.go:257` | `deliver` | 普通 | `msg.ID`、`pending` |
| `internal/gateway/session.go:263` | `deliver` | 普通 | 向 pending channel 发送 |
| `internal/gateway/session.go:207` | `request` | 普通 | 收到 `res` |
| `internal/gateway/gateway.go:312` | `handleCall` | `resp.Error != nil` | 协议错误 |
| `internal/gateway/gateway.go:318` | `handleCall` | `len(resp.Result)==0` | 空结果 |
| `internal/gateway/gateway.go:324` | `handleCall` | 普通 | 成功状态码 |
| `internal/gateway/gateway.go:326` | `handleCall` | 普通 | 写 HTTP body |

---

## 11. 错误分支全覆盖（每个 `if` 都有触发方式）

### 11.1 网关

| 分支 | 位置 | 触发请求 | 预期 |
|---|---|---|---|
| 未鉴权 | `gateway.go:194` | 不带 Authorization | 401 |
| JSON 解码失败/未知字段 | `gateway.go:203` | body 加 `"typo":1` | 400 `-32602` |
| name 为空 | `gateway.go:207` | `{"name":""}` | 400 `-32602` |
| call_id 非法 | `gateway.go:215` | `"call_id":"bad/id"` | 400 `-32602` |
| 设备离线 | `gateway.go:220` | 停掉 daemon 后调用 | 404 `-32011` |
| workspace 格式非法 | `gateway.go:231` | `"workspace_id":"bad/id"` | 400 `-32602` |
| workspace 未注册 | `gateway.go:237` | `"workspace_id":"ws_missing"` | 400 `-32012` |
| session 非法 | `gateway.go:243` | `"session_id":"a\u0000b"` | 400 `-32602` |
| 客户端断开 | `gateway.go:296` | curl `--max-time 1` | 不写响应；daemon 收到 cancel |
| 网关超时 | `gateway.go:299` | 假设备不回复/daemon 暂停 | 504 `-32001` |
| 设备断开 | `gateway.go:302` | 调用中杀 daemon | 502 `-32011` |
| daemon 协议错误 | `gateway.go:312` | 未知工具/工作区/参数 | 400/409/429 按 `httpStatusForCode` |
| 空结果 | `gateway.go:318` | 假设备返回空 result | 502 `-32603` |
| 缺少 device id | `gateway.go:326` | WS 不带 `X-Termio-Device` | 400 |
| WS token 错误 | `gateway.go:333` | WS 错误 token | 401 |
| 注册超时 | `gateway.go:365` | 连接后不发注册帧 | 4004 |
| 非文本注册帧 | `gateway.go:371` | 发送 binary frame | policy violation |
| 注册 JSON 错误 | `gateway.go:376` | 发送 `not json` | policy violation |
| 方法/ID 错误 | `gateway.go:380` | 发 `tools/list` 作为首帧 | policy violation |
| 设备 ID 不匹配 | `gateway.go:390` | header 与 params.device.id 不同 | 4001 |
| 协议版本不支持 | `gateway.go:395` | `protocol_version=99` | 4002 |
| 工作区注册非法 | `gateway.go:402` | 重复 ID/缺失默认 | 注册错误 |
| 旧连接被取代 | `gateway.go:436` | 两个相同 device_id | 旧连接 4003 |

### 11.2 daemon

| 分支 | 位置 | 触发方式 | 预期 |
|---|---|---|---|
| 参数解码失败 | `dispatcher.go:86` | 直接 WS 发坏 params | -32602 |
| 工具名为空 | `dispatcher.go:90` | 直接 WS | -32602 |
| 缺 callId | `dispatcher.go:95` | 直接 WS | -32602 |
| callId 非法 | `dispatcher.go:99` | 直接 WS 含控制字符 | -32602 |
| session 非法 | `dispatcher.go:105` | 直接 WS 含 NUL | -32602 |
| 未知工作区 | `dispatcher.go:110` | 直接 WS `workspaceId=ws_missing` | -32012 |
| 缓存指纹冲突 | `dispatcher.go:124` | 同 callId 改参数 | -32602 |
| 工作区忙 | `dispatcher.go:136` | 并发 > `max_concurrent_calls` | -32010 |
| 同 key 在途 | `dispatcher.go:146` | 并发同 callId | -32005 |
| 竞态缓存 | `dispatcher.go:152` | 并发快路径/reserve | 重放 |
| 地端超时 | `dispatcher.go:204` | 慢工具 + 小 timeout | isError |
| 工具参数错误 | `dispatcher.go:216` | 未知字段/缺 pattern | -32602 |
| 未知工具 | `dispatcher.go:216` | `does_not_exist` | -32602 |
| 工具内部错误 | `dispatcher.go:220` | 路径越界/文件不存在/权限 | isError |
| 工具 panic | `dispatcher.go:186` | 只有未来工具会 panic | isError `internal tool error` |
| 结果编码失败 | `dispatcher.go:227` | 当前类型不会发生 | 防御分支 |
| 未知方法 | `daemon.go:274` | 直接 WS 发 `foo/bar` | -32601 |
| 心跳失败 | `daemon.go:229` | 阻断网络/杀网关 | 重连 |
| 被取代 | `daemon.go:135` | 第二个同 device daemon | 4003 + 冷却 |

### 11.3 工具/沙箱

| 分支 | 位置 | 触发 |
|---|---|---|
| pattern 为空 | `search_files.go:80` | `arguments:{}` |
| pattern 过长 | `search_files.go:83` | 257 字节 |
| max_results/max_depth 负数 | `search_files.go:86/89` | 负值 |
| path 不是目录 | `search_files.go:118` | 指向文件 |
| 相对路径 `..` 逃逸 | `sandbox.go:140` | `"path":"../outside"` |
| `.termio` deny | `sandbox.go:154` | `"path":".termio/config.json"` |
| 根外绝对路径 | `sandbox.go:176` | `C:/Windows/win.ini` |
| UNC/ADS | `sandbox.go:305` | `\\\\server\\share` / `C:file:stream` |
| 搜索时 ctx 取消 | `search_files.go:134` | curl 断开/超时 |
| 隐藏目录跳过 | `search_files.go:180` | 默认 `include_hidden=false` |
| symlink 跳过 | `search_files.go:177` | 创建 symlink（Windows 需权限） |
| max_results 截断 | `search_files.go:208` | `max_results:1` 且多文件 |
| read_file 空 path | `read_file.go:64` | `{"path":""}` |
| read_file 非普通文件 | `read_file.go:92` | 指向目录 |
| read_file 截断 | `read_file.go:110` | `max_bytes` 小于文件 |
| read_file 二进制 | `read_file.go:115` | 读 `bin/termio.exe` |
| read_file ctx 取消 | `read_file.go:96` | 大文件+断开 |
| 未知名 | `registry.go:77` | `does_not_exist` |
| 参数未知字段 | `registry.go:87` | 多传字段 |

---

## 12. 重连、心跳、关闭断点

| 位置 | 函数 | 触发 | 看到什么 |
|---|---|---|---|
| `internal/daemon/daemon.go:56` | `Run` | 启动/断线 | 重连循环 |
| `internal/daemon/daemon.go:72` | `Run` | 收到 4003 | `superseded` 冷却 |
| `internal/daemon/daemon.go:76` | `Run` | 连接结束 | `retry_in` |
| `internal/daemon/daemon.go:85` | `Run` | Ctrl+C | ctx 取消 |
| `internal/daemon/daemon.go:104` | `runOnce` | 每次拨号 | dial 错误/HTTP 状态 |
| `internal/daemon/daemon.go:128` | `runOnce` | 注册成功 | 心跳 goroutine |
| `internal/daemon/daemon.go:130` | `runOnce` | 注册成功 | dispatcher 创建 |
| `internal/daemon/daemon.go:131` | `runOnce` | 连接结束 | `disp.shutdown()` |
| `internal/daemon/daemon.go:218` | `heartbeat` | 每 20s | ping |
| `internal/daemon/daemon.go:229` | `heartbeat` | ping 失败 | 取消 session |
| `internal/gateway/session.go:292` | `heartbeat` | 每 20s | 网关 ping |
| `internal/gateway/session.go:301` | `heartbeat` | ping 失败 | close |
| `internal/gateway/session.go:283` | `close` | 任意断线 | `done`、failPending、hub.remove |
| `internal/gateway/session.go:270` | `failPending` | 设备断开 | 所有 pending 收到 `errDeviceClosed` |
| `internal/daemon/dispatcher.go:363` | `shutdown` | daemon 退出 | 所有在途调用 cancel |
| `internal/daemon/workspace.go:165` | `Close` | daemon 退出 | 关闭每个 os.Root |

**触发重连：**

- 停网关：daemon 进入 1s→30s 退避；重启网关后重新注册。
- 启动第二个相同 `device_id` 的 daemon：旧连接收到 4003，冷却 5 分钟。
- 调用中按 Ctrl+C 停 daemon：观察 `errShutdown`、`failPending`、网关 502。

---

## 13. tools/list 刷新断点

这条链路以前在参考网关里不会触发，现在已经用 `?refresh=1` 接通：

| 位置 | 函数 | 触发 | 看到什么 |
|---|---|---|---|
| `internal/gateway/gateway.go:129` | `handleTools` | Postman **Refresh tools** | `refresh` query |
| `internal/gateway/gateway.go:143` | `handleTools` | 同上 | `dc.refreshTools` |
| `internal/gateway/session.go:155` | `refreshTools` | 同上 | 构造 `tools/list` 请求 |
| `internal/gateway/session.go:177` | `request` | 同上 | pending 注册 |
| `internal/daemon/daemon.go:265` | `readLoop` | 同上 | 进入 tools/list 分支 |
| `internal/daemon/daemon.go:287` | `handleToolsList` | 同上 | `workspaces.Specs()` |
| `internal/gateway/session.go:257` | `deliver` | 同上 | 结果配对 |
| `internal/gateway/gateway.go:155` | `handleTools` | 同上 | 刷新后的 snapshot |

---

## 14. 配置、CLI、客户端断点（可选）

### 14.1 配置

| 位置 | 函数 | 触发 |
|---|---|---|
| `internal/config/config.go:178` | `Load` | 启动/CLI 读配置 |
| `internal/config/config.go:209` | `normalizeWorkspaces` | 旧配置迁移、路径规范化 |
| `internal/config/config.go:481` | `Validate` | 任何非法配置 |
| `internal/config/config.go:725` | `Save` | init/workspace add/remove/default/reset |
| `internal/config/config.go:425` | `LoadDefault` | 旧配置迁移 |

### 14.2 CLI

| 位置 | 函数 | 怎么调试 |
|---|---|---|
| `cmd/termio/main.go:33` | `run` | VS Code 调试 `cmd/termio`，args 填 `workspace list` |
| `cmd/termio/main.go:186` | `runDaemon` | F5 `Termio Daemon` |
| `cmd/termio/main.go:357` | `runWorkspaceList` | args `workspace list` |
| `cmd/termio/main.go:385` | `runWorkspaceShow` | args `workspace show` |
| `cmd/termio/main.go:437` | `runWorkspaceAdd` | args `workspace add ...` |
| `cmd/termio/main.go:469` | `runWorkspaceRemove` | args `workspace remove ...` |
| `cmd/termio/main.go:493` | `runWorkspaceDefault` | args `workspace default ...` |
| `cmd/termio/main.go:520` | `runWorkspaceSet` | args `workspace set ...` |
| `cmd/termio/main.go:559` | `runWorkspaceReset` | args `workspace reset` |

在 `.vscode/launch.json` 里复制一个 Go 配置，把 `program` 指向 `${workspaceFolder}/cmd/termio`，`args` 写成对应命令即可；或者直接在终端跑 `dlv debug ./cmd/termio -- workspace list`。

### 14.3 termioctl

| 位置 | 函数 | 看到什么 |
|---|---|---|
| `cmd/termioctl/main.go:232` | `send` | 最终 HTTP body |
| `cmd/termioctl/main.go:478` | `parseCall` | 普通 JSON / JSON-RPC envelope |
| `cmd/termioctl/main.go:335` | `repl` | 交互命令 |
| `cmd/termioctl/main.go:375` | `command` | `:workspace`/`:session` 等 |

F5 **Termio CLI (REPL)** 可在集成终端输入 JSON，断点停在 `send`。

---

## 15. 一次正常调用应看到的完整数据流

用 Postman **System info**（默认工作区）或 **Workspace call**（`ws_docs`），按顺序验证：

```text
[HTTP]
gateway.handleCall entry
  body / deviceID / token
  body decode
  workspaceID（空→默认 / 显式）
  dc.hasWorkspace
  ToolCallParams
  dc.request
  d.pending[g_id]
  WriteJSON

[WS → daemon]
daemon.readLoop conn.Read
  msg.Method == tools/call
  go dispatcher.handleCall
     DecodeParams
     WorkspaceSet.Get
     callKey
     lookupCache
     ws.Sem
     reserve
     ws.Registry.Call
       Registry.Call
       Sandbox.Resolve
       tool.Call
     result.Meta
     cachePut
     dispatcher.send
       WriteJSON

[WS → gateway]
gateway.deviceConn.readLoop
  msg.ID != ""
  deliver
  pending channel
  request returns
  resp.Error / resp.Result
  HTTP 200 / 400 / 502 / 504
```

正常成功路径应返回：

```json
{
  "content": [...],
  "structuredContent": {...},
  "_meta": {
    "termio/callId": "...",
    "termio/workspaceId": "ws_default 或 ws_docs",
    "termio/sessionId": "..."
  }
}
```

---

## 16. 死角审计：哪些分支不是正常 Postman 能触发

下面这些分支**不是无用代码**，而是失败/恢复/协议防御路径；正常请求不会经过，必须用指定方式触发：

| 分支 | 为什么不走正常路径 | 如何触发 |
|---|---|---|
| daemon `tools/list` | 参考网关默认只用注册时的缓存 | Postman **Refresh tools**（`/tools?refresh=1`） |
| daemon 未知方法 `-32601` | 参考网关只发 tools/call/tools/list/cancelled | 直接 WebSocket 发 `{"method":"foo/bar","id":"1"}` |
| daemon 非 tools/call 响应 | 参考网关不发无效请求 | 自定义 WS 客户端/假设备 |
| gateway 设备主动请求分支 | 当前 daemon 不向网关发请求 | 自定义 daemon/Postman 假设备 |
| gateway 空 result | 真实工具总会返回 content | 假设备回复 `{}` |
| dispatcher 工具 panic | 内置工具不 panic | 仅未来工具/故意注入 panic 的测试工具 |
| dispatcher `reserveMismatch`/`reserveCached` | 是快路径与 reserve 之间的并发竞态保护 | 并发同 key 不同参数/同 key 重试，或测试中构造竞态 |
| dispatcher 结果编码失败 | `CallToolResult` 总能 JSON 编码 | 只有未来加入不可编码字段才会发生 |
| `CloseOverloaded` 等常量 | 协议保留值，没有运行时分支 | 不会“命中”；它们是常量，不是代码路径 |
| `search_files` walkErr/权限 | 需要不可读目录 | 创建无权限/损坏目录；Windows ACL 或 Linux chmod |
| `search_files` symlink | 需要 symlink | `mklink`/`ln -s` 后搜索 |
| `read_file` 二进制 | 需要非文本文件 | 读 `bin/termio.exe` |
| 配置校验错误分支 | 正常配置不会触发 | 手工改坏 config / 旧配置迁移 |

如果你希望连这些**防御分支也删掉**，那会把容错和协议兼容一起删掉；我的建议是保留，并在调试时把它们当作“异常路径”验证，而不是死代码。真正没有入口的代码已经在本轮补齐/暴露：

- `tools/list` 已通过 `?refresh=1` 接通；
- `max_inflight` 已在 `/v1/devices` 和 `/tools` 返回；
- 工作区路由、会话、幂等域都有真实请求路径。

---

## 17. 建议的调试顺序（照着做）

1. 按 [debug-full-chain.md](debug-full-chain.md) 1.1 或 1.2 启动。
2. 先只打普通断点：
   - `gateway.go:192`
   - `session.go:177`
   - `daemon.go:241`
   - `dispatcher.go:84`
   - `workspace.go:122`
   - `sandbox.go:132`
   - `system_info.go:37`
   - `dispatcher.go:232`
   - `session.go:257`
   - `gateway.go:324`
3. 用 Postman **System info** 跑一遍；确认这 10 个点都能命中。
4. 换成 **Workspace call**（`ws_docs`），在每个点 Watch `workspaceID`/`ws.Sandbox.Workspace()`，确认路由没串。
5. 加 `callID` 条件断点，跑 **Duplicate call_id**，确认 `lookupCache` 命中、没有进工具。
6. 加 `sessionID` 条件断点，跑同工作区两个 session，确认 `key` 不同。
7. 再按第 11 节触发错误分支；每触发一个，就在表里打勾。
8. 最后用 **Refresh tools** 命中 `handleToolsList`，确认没有剩余“从未经过”的运行时分支。

这样一轮下来，正常链路、工作区链路、会话链路、幂等链路、错误链路、重连链路都能在调试器里看到实际数据。

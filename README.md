# Termio

**云端 Agent 的地端工具执行层。**

云端 Agent 作为基础服务，同时服务多个租户；但租户的真实文件、项目和环境在各自的电脑上。Termio 让云端 Agent 决定“调用什么工具”，把执行安全地转发到用户本机，由地端端子真正执行并回传结果。

- 地端主动外连，不需要公网 IP、端口映射或入站防火墙规则
- Go 单静态二进制，Windows / macOS / Linux（amd64 / arm64）
- 工具方法/结果遵循 MCP 形状，接入现有 Agent 框架成本低
- 只读工具 + `os.Root` 沙箱 + 每设备 token，默认最小权限
- 支持多个本地工作区：一个工作区可服务多个会话/任务，不同工作区可并发执行；切换已注册工作区无需重启
- 默认在 `~/.lares` 工作区下工作；端子信息在 `~/.lares/.termio`（工具不可读写）；工作区只能由本地用户配置
- 自带单实例参考网关，开箱即可端到端联调

## 背景与问题

云端 Agent 的工具实现天然运行在云端：

```
LLM 决定调用 search_files
  └─ 云端执行 search_files
       └─ 搜的是云端的文件系统，不是用户的
```

要操作租户本机，常见做法都有明显代价：

| 方案 | 主要问题 |
|---|---|
| 云端直接执行 | 执行在错误的机器上，拿不到用户文件 |
| 本地开 HTTP/MCP Server + 端口映射 / frp / Cloudflare Tunnel | 需要入站或隧道；把整台机器的服务暴露出去；每设备配置和运维复杂 |
| Kafka / RabbitMQ | 运维重；请求-响应、取消、流式进度都要自己造；仍然解决不了“谁持有地端连接” |
| MQTT 5 | 弱网/离线/海量设备很合适；但交互式请求-响应、取消和进度需要额外设计，也没有工具 schema 标准 |
| 自研“每个工具一个 RPC” | 没有统一的工具描述和结果形状，N 个工具就是 N 套接入 |
| **Termio** | 出站 WebSocket + 工具级白名单 + MCP 形状 + 单二进制 + 用户级权限 + 沙箱 |

MQTT 仍是 Termio 的合理备选（尤其是离线队列/海量弱网设备）；但 v1 优先选择 WebSocket，因为它对**双向交互、取消、进度和低延迟**最直接。

## 架构

```
┌──────────────────────────── 云端 ────────────────────────────┐
│  Cloud Agent（多租户）                                         │
│    └─ tool stub ── HTTP ──▶ termio-gateway                    │
│                              ├─ 设备鉴权 / 路由 / pending call │
│                              └─ 持有地端 WebSocket 会话        │
└───────────────────────────────────┬───────────────────────────┘
                                    │ 出站长连接（wss）
┌───────────────────────────────────▼───────────────────────────┐
│  地端 termio daemon                                            │
│    ├─ 用户级自启动 / 配置 / 日志                               │
│    ├─ 工作区注册表（默认 + 多个本地授权目录）                  │
│    ├─ 每工作区 os.Root 沙箱 + 工具注册表                       │
│    ├─ search_files / read_file / system_info                   │
│    └─ 并发、超时、取消、进度、按(工作区,会话,callId)幂等缓存   │
└────────────────────────────────────────────────────────────────┘
```

一次调用：

1. 云端 Agent 的 tool stub 发 `POST /v1/devices/{id}/tools/call`。
2. 网关把请求编码成 `tools/call`，填入设备默认工作区（或使用调用指定的 `workspace_id`），通过该设备持有的 WebSocket 推下去。
3. 地端按 `workspace_id` 选择沙箱和工具注册表，在对应工作区内执行工具，回传 progress 和最终结果。
4. 网关把结果作为 HTTP 响应返回给云端 Agent；结果 `_meta` 带回工作区和会话 ID。

**多租户隔离点**：

- 网关按 `device_id` 路由；每台设备可有独立 token。
- 地端为每个已注册工作区打开独立沙箱；一次调用只能落在其中一个，云端无法指定任意路径。
- 一个工作区可服务多个会话/任务；幂等缓存按 `(workspace, session, callId)` 隔离，工作区之间可并发。
- 地端只注册只读工具；没有 shell、写文件、删除或进程管理。
- 云端 Agent 按租户上下文选择设备，模型不能自由指定任意 device_id。

v1 的 `termio-gateway` 是单实例参考实现：内存态、静态设备表、无 HA/限流/审计。多实例扩展路径（presence + NATS subject-per-device）和完整生产化清单见 [docs/architecture.md](docs/architecture.md) 与 [docs/security.md](docs/security.md)。

## 技术亮点

- **反向 WebSocket**：地端出站即可穿透 NAT；一条连接多路复用，支持低延迟请求/响应、进度和取消。
- **MCP 形状的工具契约**：`tools/list`、`tools/call`、`content`/`structuredContent`/`isError`；云端工具接入成本低。
- **逐跳 id + 端到端 callId**：JSON-RPC `id` 只在本连接内配对；`_meta.termio/callId` 跨重连/重试保持幂等，地端缓存 10 分钟。
- **相对超时**：`_meta.termio/timeoutMs` 避免云端/地端时钟偏差；网关多等 2 秒让地端超时结果先赢。
- **可靠连接**：双向心跳、指数退避重连、4003 取代冷却、并发上限、过载快速失败。
- **多工作区路由**：`workspace_id` 选择本地授权目录；一个工作区多个会话，工作区之间独立沙箱与并发额度；热切换不需要重启。
- **`os.Root` 沙箱**：workspace + 额外根、绝对/相对路径、符号链接/junction 逃逸拦截、UNC/ADS 拒绝；不是简单的字符串前缀检查。
- **用户级自启动**：Linux systemd user / macOS LaunchAgent / Windows 计划任务；避免 LocalSystem/root 读不到用户文件。
- **单静态二进制**：除 `coder/websocket` 外只用标准库；三端交叉编译简单。
- **可二开**：一个 `Tool` 接口即可新增能力，教程见 [docs/tools.md](docs/tools.md)。

## 快速开始

需要 Go 1.24+。

```bash
make build
# 或
go build -o bin/termio ./cmd/termio
go build -o bin/termio-gateway ./cmd/termio-gateway
go build -o bin/termioctl ./cmd/termioctl
```

启动参考网关：

```bash
./bin/termio-gateway --addr 127.0.0.1:8787 --token dev-token
```

初始化并运行地端（workspace 换成你想允许的目录）：

```bash
./bin/termio init --token dev-token --workspace "$PWD"
./bin/termio run

# 可选：再注册一个本地工作区；重启 daemon 后生效
./bin/termio workspace add --id ws_docs --name docs "$HOME/docs"
./bin/termio workspace list
```

终端 JSON 自测（启动后另开一个终端）：

```bash
./bin/termioctl --gateway http://127.0.0.1:8787 --token dev-token
# 然后输入：
# {"name":"system_info","arguments":{}}
# {"name":"search_files","arguments":{"pattern":"*.go","max_results":10}}
```

详细命令、JSON-RPC envelope、管道模式和 Postman 用法见 [docs/self-test.md](docs/self-test.md)。

也可以直接用 curl 调网关：

```bash
curl -H "Authorization: Bearer dev-token" http://127.0.0.1:8787/v1/devices

curl -X POST "http://127.0.0.1:8787/v1/devices/<device_id>/tools/call" \
  -H "Authorization: Bearer dev-token" \
  -H "Content-Type: application/json" \
  -d '{"name":"search_files","arguments":{"pattern":"*.go","max_results":10}}'
```

Windows 下把 `./bin/termio` 换成 `.\bin\termio.exe`；PowerShell 调用示例见 [docs/operations.md](docs/operations.md)。

生产部署、`wss://`、自启动、日志和排障：见 [docs/operations.md](docs/operations.md)。

## 接入自己的云端 Agent

云端不需要引入 Termio SDK，只需要一个 HTTP tool stub：

```python
import json, urllib.request

GATEWAY = "https://gateway.example.com"
DEVICE_ID = "dev_..."
TOKEN = "cloud-or-device-token"

def call_tool(name, arguments, timeout_ms=30000, call_id=None,
              workspace_id=None, session_id=None):
    body = {"name": name, "arguments": arguments, "timeout_ms": timeout_ms}
    if call_id:
        body["call_id"] = call_id
    if workspace_id:
        body["workspace_id"] = workspace_id
    if session_id:
        body["session_id"] = session_id
    req = urllib.request.Request(
        f"{GATEWAY}/v1/devices/{DEVICE_ID}/tools/call",
        data=json.dumps(body).encode(),
        headers={
            "Authorization": f"Bearer {TOKEN}",
            "Content-Type": "application/json",
        },
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=timeout_ms / 1000 + 5) as resp:
        result = json.load(resp)
    if result.get("isError"):
        raise RuntimeError(result["content"][0]["text"])
    return result.get("structuredContent", {})
```

要点：

- 重试复用同一个 `call_id`；地端会在同一 `(workspace_id, session_id)` 下重放已完成结果。
- `isError: true` 是工具失败，不是网络失败；把错误文本交给模型决策。
- 设备 ID 从租户上下文解析，不要交给 LLM 自由选择。
- 会话/任务绑定一个 `workspace_id`；同一工作区可以有多个 `session_id`，工作区之间可并发。
- 工具列表可通过 `GET /v1/devices/{id}/tools` 发现；工作区列表通过 `GET /v1/devices/{id}/workspaces` 发现。

完整说明：[docs/cloud-agent-integration.md](docs/cloud-agent-integration.md)。可直接导入的 Postman 集合：[docs/postman/termio.postman_collection.json](docs/postman/termio.postman_collection.json)。

## 调试

仓库自带 VS Code 调试配置：

- `F5` → `Termio: Gateway + Daemon` 一键启动，自动生成 `.debug/config.json`（`ws_default` + `ws_docs` 两个工作区）和测试文件
- 也可以 PowerShell 起网关、VS Code 只 F5 调试 `Termio Daemon`，Postman 发请求
- 断点覆盖注册、HTTP→WS 转发、工作区路由、调度、沙箱和工具执行
- Postman 可以导入 HTTP 集合；也可以**把 Postman 当成一台假设备**，在 WebSocket 里手动接收网关推送并回复

完整教程（含主动推消息、接消息、进度、超时/取消的原始帧）：[docs/debugging.md](docs/debugging.md)。

想按“从进程启动到一条消息返回”的顺序逐点调试，看 [docs/debug-full-chain.md](docs/debug-full-chain.md)：每个功能点对应哪些文件/函数、在哪里下断点、如何验证多工作区与会话并发。

## 文档

| 文档 | 内容 |
|---|---|
| [docs/architecture.md](docs/architecture.md) | 组件、连接生命周期、部署拓扑、多租户、UI 选型、多实例扩展 |
| [docs/workspace.md](docs/workspace.md) | 工作区/会话模型、多工作区配置、路由、热切换、并发与安全边界 |
| [docs/roadmap.md](docs/roadmap.md) | V1.x / V2 / V3 计划与验收标准 |
| [docs/protocol.md](docs/protocol.md) | WebSocket/JSON-RPC、HTTP API、错误码、幂等、限额 |
| [docs/tools.md](docs/tools.md) | 内置工具、沙箱、**新增工具（二开）教程** |
| [docs/security.md](docs/security.md) | 威胁模型、认证、沙箱边界、生产化清单 |
| [docs/operations.md](docs/operations.md) | 配置、CLI、三端自启动、网关、构建、排障 |
| [docs/cloud-agent-integration.md](docs/cloud-agent-integration.md) | 云端 Agent 接入细节 |
| [docs/debugging.md](docs/debugging.md) | VS Code 断点调试 + Postman/WebSocket 联调 |
| [docs/debug-full-chain.md](docs/debug-full-chain.md) | **全链路调试**：启动 → 注册 → 消息 → 工作区路由 → 工具 → 响应，逐功能点代码路径与断点 |
| [docs/self-test.md](docs/self-test.md) | 用 termioctl 在终端输入 JSON 自测；Postman 用法 |

## 当前状态

- v1 提供三个只读工具，不包含 shell/写操作。
- 多工作区已实现：默认 `~/.lares`，端子信息在 `~/.lares/.termio`（沙箱 deny，工具不可读写）；用户可注册多个本地目录，会话按 `workspace_id` 路由。新增/删除工作区需重启 daemon，已注册工作区之间切换无需重启。见 [docs/workspace.md](docs/workspace.md)。
- `termio-gateway` 是**参考实现**：单实例、内存态，不适合直接作为公网多租户服务。
- 设备配对/OAuth、mTLS、token 轮换、端到端加密、写操作审批、离线任务队列尚未实现；生产化清单见安全文档。
- Windows 已做本机端到端验证；macOS/Linux 产物已交叉编译，尚未在真实机器上运行验证。
- 详细边界见 [docs/architecture.md](docs/architecture.md)，后续计划见 [docs/roadmap.md](docs/roadmap.md)。

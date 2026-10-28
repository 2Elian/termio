# 接入自己的云端 Agent

Termio 不绑定 Agent 框架。云端只需要一个能发 HTTP 的工具实现：Agent 决定调用工具，stub 把调用发给网关，网关转发到地端。

```
Agent LLM ──tool_use──▶ 你的 tool stub ──HTTP──▶ termio-gateway ──WS──▶ 地端 termio
                              ▲                                          │
                              └──────────── CallToolResult ───────────────┘
```

## 前置条件

- 网关地址（生产为 `https://` + `wss://`）
- 云端可用的 Bearer token
- 目标设备已在线，且知道其 `device_id`
- 网关的 `/v1/devices` 能看到该设备、工具列表和工作区列表
- 云端会话/任务已经决定使用哪个 `workspace_id`（省略则用设备默认工作区）

## HTTP 契约

```http
POST /v1/devices/{device_id}/tools/call
Authorization: Bearer <token>
Content-Type: application/json
```

```json
{
  "name": "search_files",
  "arguments": {"pattern": "*.go", "max_results": 20},
  "timeout_ms": 30000,
  "call_id": "stable-id-for-retries",
  "workspace_id": "ws_docs",
  "session_id": "chat-42"
}
```

- `call_id` 可选。**重试时必须复用**；新的逻辑调用应生成新 ID。
- `workspace_id` 可选；省略时网关使用设备注册的默认工作区。未知 ID 返回 400 + `-32012`。
- `session_id` 可选；同一工作区可以有多个会话。它只用于关联/幂等作用域，不参与鉴权。
- 幂等重试必须保持 `(workspace_id, session_id, call_id)` 一致，否则会被当成新的调用。
- `timeout_ms` 可选；最终超时还会受网关配置约束。
- 成功返回 MCP `CallToolResult`；工具失败是 HTTP 200 + `isError: true`。
- 协议错误返回非 2xx + `{"error":{"code":...,"message":"..."}}`。

完整字段和错误码见 [protocol.md](protocol.md)。

## 最小 Python 客户端

```python
import json
import urllib.request

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
        data=json.dumps(body).encode("utf-8"),
        headers={
            "Authorization": f"Bearer {TOKEN}",
            "Content-Type": "application/json",
        },
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=timeout_ms / 1000 + 5) as resp:
        return json.load(resp)


def search_files(pattern, path=None, max_results=50, call_id=None,
                 workspace_id=None, session_id=None):
    args = {"pattern": pattern, "max_results": max_results}
    if path:
        args["path"] = path
    result = call_tool("search_files", args, call_id=call_id,
                       workspace_id=workspace_id, session_id=session_id)
    if result.get("isError"):
        raise RuntimeError(result["content"][0]["text"])
    return result.get("structuredContent", {})
```

完整示例在 `examples/cloud_tool_stub.py`。

## 工具与工作区发现

```http
GET /v1/devices/{device_id}/tools
GET /v1/devices/{device_id}/workspaces
```

`/workspaces` 返回：

```json
{
  "device_id": "dev_...",
  "workspaces": [
    {"id": "ws_default", "name": "default"},
    {"id": "ws_docs", "name": "docs"}
  ],
  "default_workspace_id": "ws_default"
}
```

- 只暴露 ID/名称，本地路径永远不会返回给云端。
- 会话/任务应固定自己的 `workspace_id`；不要在同一条任务里无理由来回切换。
- 一个工作区可以有多个会话；需要区分对话时传不同的 `session_id`。
- 新增/删除工作区由本地用户操作，daemon 重启后云端重新拉取列表。

工具列表返回：

```json
{
  "device_id": "dev_...",
  "tools": [
    {
      "name": "search_files",
      "description": "...",
      "inputSchema": {"type": "object", "properties": {...}}
    }
  ]
}
```

映射到不同框架：

```python
# Anthropic tool use
anthropic_tools = [
    {"name": t["name"], "description": t["description"], "input_schema": t["inputSchema"]}
    for t in tools
]

# OpenAI function calling
openai_tools = [
    {"type": "function", "function": {
        "name": t["name"], "description": t["description"], "parameters": t["inputSchema"]
    }}
    for t in tools
]
```

建议：

- 按租户/设备过滤工具，不要把整台设备的能力无差别暴露给模型。
- 设备 ID 来自租户上下文，不要让 LLM 自由选择任意 device_id。
- 工具名、描述、schema 保持与地端一致；工具改名属于破坏性变更。
- 云端可以缓存工具列表，但设备重连后可能变化。

## 错误处理

### 工具失败（HTTP 200 + `isError: true`）

```json
{
  "content": [{"type": "text", "text": "path is outside the allowed roots"}],
  "isError": true
}
```

把 `content[0].text` 作为工具错误返回给模型，让模型决定下一步；不要把整个请求标成网络错误。

### 协议/传输错误

| HTTP | 含义 | 建议 |
|---|---|---|
| 400 | 参数错误、未知工具 | 修正调用；不要重试 |
| 401 | token 无效 | 检查凭证；不要重试 |
| 404 | 设备不在线 | 重试或提示用户启动地端 |
| 409 | 取消 / callId 正在执行 | 复用 callId 稍后重试，或等待原调用完成 |
| 429 | 地端并发已满 | 指数退避后重试 |
| 502 | 地端断开/内部错误 | 短暂退避后重试 |
| 504 | 网关等待超时 | 检查任务耗时；可重试或缩小搜索范围 |

重试规则：

- 网络错误、502、504、429 可以重试。
- 重试**复用同一个 `(workspace_id, session_id, call_id)`**；地端在 10 分钟内会重放该键的已完成结果。
- 4xx（除 429）通常不要重试。
- 超时后如果换新 callId，可能重复执行；只读工具重复执行通常无害，但仍应尽量复用。

## 超时预算

- 地端默认 30s；网络调用和 scan 大目录可能更久。
- 云端 HTTP 客户端的超时应大于 `timeout_ms`（网关自身等待 `timeout + 2s`）。
- Agent 框架的工具超时应再大一点，避免框架先取消、网关还在等待。
- 对长任务，优先缩小搜索范围（`path`、`max_depth`、`max_results`），而不是无限加大超时。

## 多设备路由

```python
def device_for_tenant(tenant_id: str) -> str:
    # 从租户/用户绑定关系读取，不要相信模型输入
    ...
```

- 每次工具调用都从受信任的上下文解析 device_id。
- 按设备 token 鉴权时，网关只允许访问对应设备；共享 token 不具备租户隔离，仅开发用。
- 一个租户有多台设备时，在云端做设备选择（例如按平台、在线状态、用户选择），不要靠地端猜测。

## 进度与流式结果

v1 的 HTTP API 是同步的：

- 地端会发 progress，但网关只写 debug 日志，不通过 HTTP 流式返回。
- 最终结果一次性返回。
- 如果 Agent 需要“正在扫描”的反馈，可以在云端先返回“已开始”的 UI 事件，再等待工具结果；真正的 progress 流式推送是后续版本（SSE/WS）。

## 安全

- token 只放在云端服务端，不下发浏览器/桌面客户端。
- 使用 HTTPS/WSS；不要在 URL query 里放 token。
- 云端日志不要记录 token、完整文件内容或敏感路径。
- 只向模型暴露业务需要的工具。
- 对 `read_file` 的结果做必要的脱敏/截断，不要把整个文件塞进模型上下文。

## 用 Postman 联调

导入 [postman/termio.postman_collection.json](postman/termio.postman_collection.json)，设置：

- `baseUrl` = `http://127.0.0.1:8787`
- `token` = 网关 token
- 先执行 **List devices**，测试脚本会自动把第一台设备写入 `deviceId`；再执行 **List workspaces** 获取 `workspaceId`
- 集合里有带 `workspace_id`/`session_id` 的调用示例和未知工作区用例

更完整的“Postman 当假设备、手动接收推送消息”的教程见 [debugging.md](debugging.md)。

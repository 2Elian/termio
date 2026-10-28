# 工具与二次开发

## 工具接口

所有工具实现 `internal/tools.Tool`：

```go
type ProgressFunc func(progress, total float64, message string)

type Tool interface {
    Spec() protocol.ToolSpec
    Call(ctx context.Context, args json.RawMessage, progress ProgressFunc) (protocol.CallToolResult, error)
}
```

- `Spec` 返回工具名、描述和 JSON Schema；地端启动/注册时上报给网关。
- `Call` 的 `ctx` 会在地端超时、云端取消、连接关闭时被取消；长循环必须定期检查。
- 工具执行失败返回普通 `error`，由调度器转换成 `isError: true` 的结果。
- 参数错误返回 `*tools.ArgError`，调度器转换成 JSON-RPC `-32602`。
- 未知工具由 registry 返回 `*tools.UnknownToolError`，同样是 `-32602`。
- 成功结果通过 `textResult(text, structured)` 构造，包含一个文本块和结构化数据。

## 内置工具

> **工作区**：每次调用通过 `workspace_id` 选择一个本地工作区（省略时用默认，默认 `~/.lares`）；工具看到的 registry/sandbox 都属于该工作区。省略 `path` 时使用该工作区；相对路径相对该工作区解析，且 `..` 不能逃出工作区。完整语义见 [workspace.md](workspace.md)。

### `search_files`

按**文件名**搜索，不搜索文件内容。

| 参数 | 类型 | 说明 |
|---|---|---|
| `path` | string? | 绝对目录，或相对 workspace 的路径；省略时使用 workspace |
| `pattern` | string | 必填；子串，或对 basename 的 glob（`*.log`） |
| `max_results` | int? | 默认 200，上限 1000 |
| `include_hidden` | bool? | 默认 false；true 时包含点号条目和依赖目录 |
| `max_depth` | int? | 默认 16，上限 64 |
| `case_sensitive` | bool? | 默认 false |

默认跳过 `.git`、`.hg`、`.svn`、`node_modules`、`vendor`、`.venv`、`__pycache__`；不跟随符号链接。权限错误计入 `errors` 并跳过，不中断搜索。

返回：

```json
{
  "matches": [{"path": "C:\\work\\a.go", "size": 123, "mod_time": "2026-10-22T12:00:00Z"}],
  "truncated": false,
  "scanned": 42,
  "errors": 0,
  "root": "C:\\work"
}
```

### `read_file`

| 参数 | 类型 | 说明 |
|---|---|---|
| `path` | string | 必填；绝对路径，或相对 workspace 的路径 |
| `max_bytes` | int? | 默认 256 KiB，上限 512 KiB |

- UTF-8 文本直接放在 `content[0].text`。
- 含 NUL 或非法 UTF-8 的内容视为二进制，base64 编码后放在同一个文本块；`structuredContent` 只给元数据。
- 截断不会切断多字节 UTF-8 字符。

`structuredContent`：

```json
{"path":"...","size":123,"bytes_returned":123,"truncated":false,"is_binary":false,"encoding":"utf-8"}
```

### `system_info`

无参数，返回：

```json
{"os":"windows","arch":"amd64","hostname":"...","termio_version":"0.1.0","workspace":"C:\\Users\\alice\\.lares","allowed_roots":["C:\\Users\\alice\\.lares"]}
```

`workspace` 是本次调用所选的工作区；`allowed_roots` 是该工作区的有效根（workspace + 额外根）。不同 `workspace_id` 会得到不同的 `workspace`/`policy`。

## 沙箱

- 每个已注册工作区及其额外根都有一组 `os.Root`；一个调用只使用其 `workspace_id` 对应的 sandbox/registry。
- 空路径或相对路径以该工作区为基准；相对路径的 `..` 不能逃出该工作区。
- 绝对路径可以访问该工作区或其额外根；拒绝 `..` 越界、Windows UNC/设备路径、`C:foo` 盘符相对路径、NTFS ADS。
- `$LARES/.termio`（config/token/log）以及实际 config/log 文件是全局 deny 路径；对每个工作区都生效，`search_files` 即使 `include_hidden` 也会跳过，工作区/额外根不能设在 deny 内。
- 请求路径会先规范化（解析符号链接和 Windows 8.3 短名），再与 root 做组件级比较。
- `os.Root` 在真正打开时再次阻止符号链接/junction 逃逸，避免 check-then-open 的 TOCTOU。
- 搜索不跟随目录符号链接；`search_files` 只返回普通文件。
- 硬链接可能指向 root 外的 inode，这是已知边界；能创建硬链接的攻击者必须已有 root 内写权限。

**不要绕过 `Sandbox` 直接调用 `os.Open`/`filepath.Walk`**，否则会破坏路径边界。

## 二次开发：新增一个工具

下面以 `file_stat` 为例，演示完整流程。它返回路径的元数据。

### 1. 新建 `internal/tools/file_stat.go`

```go
package tools

import (
    "context"
    "encoding/json"
    "fmt"
    "time"

    "termio/internal/protocol"
)

type FileStat struct {
    sandbox *Sandbox
}

type fileStatArgs struct {
    Path string `json:"path"`
}

type fileStatResult struct {
    Path    string    `json:"path"`
    Size    int64     `json:"size"`
    Mode    string    `json:"mode"`
    ModTime time.Time `json:"mod_time"`
    IsDir   bool      `json:"is_dir"`
}

func (t *FileStat) Spec() protocol.ToolSpec {
    return protocol.ToolSpec{
        Name:        "file_stat",
        Description: "Return metadata for a path under the workspace or an allowed root.",
        InputSchema: json.RawMessage(`{
            "type":"object",
            "properties":{
                "path":{"type":"string","description":"Absolute path, or a path relative to the workspace."}
            },
            "required":["path"],
            "additionalProperties":false
        }`),
    }
}

func (t *FileStat) Call(ctx context.Context, raw json.RawMessage, _ ProgressFunc) (protocol.CallToolResult, error) {
    var args fileStatArgs
    if err := decodeArgs(raw, &args); err != nil {
        return protocol.CallToolResult{}, err
    }
    if args.Path == "" {
        return protocol.CallToolResult{}, &ArgError{Message: "path is required"}
    }
    select {
    case <-ctx.Done():
        return protocol.CallToolResult{}, ctx.Err()
    default:
    }

    resolved, err := t.sandbox.Resolve(args.Path)
    if err != nil {
        return protocol.CallToolResult{}, err
    }
    info, err := resolved.Entry.root.Stat(resolved.RelOS)
    if err != nil {
        return protocol.CallToolResult{}, fmt.Errorf("stat %s: %w", resolved.Abs, err)
    }

    result := fileStatResult{
        Path:    resolved.Abs,
        Size:    info.Size(),
        Mode:    info.Mode().String(),
        ModTime: info.ModTime().UTC(),
        IsDir:   info.IsDir(),
    }
    text := fmt.Sprintf("%s (%d bytes, mode %s, modified %s)",
        result.Path, result.Size, result.Mode, result.ModTime.Format(time.RFC3339))
    return textResult(text, result), nil
}
```

要点：

- 用 `decodeArgs` 解析参数；它会拒绝未知字段。
- 用 `sandbox.Resolve` 拿到 `os.Root` 和相对路径；不要自己拼接路径或用 `os.Open` 绕过沙箱。
- 用 `resolved.Entry.root.Stat/Open/FS` 访问文件。
- 检查 `ctx.Done()`；长循环至少每几百次迭代检查一次。
- 结果要限长；不要让单个工具返回超过帧限制的数据。

### 2. 注册工具

修改 `internal/tools/registry.go`：

```go
func DefaultRegistry(sb *Sandbox) *Registry {
    return NewRegistry(
        &SearchFiles{sandbox: sb},
        &ReadFile{sandbox: sb},
        &SystemInfo{sandbox: sb},
        &FileStat{sandbox: sb}, // 新增
    )
}
```

### 3. 加测试

在 `internal/tools/tools_test.go` 里复用 `newTestSandbox` 和 `jsonString`：

```go
func TestFileStat(t *testing.T) {
    root := t.TempDir()
    file := filepath.Join(root, "a.txt")
    if err := os.WriteFile(file, []byte("hello"), 0o600); err != nil {
        t.Fatal(err)
    }
    tool := &FileStat{sandbox: newTestSandbox(t, root)}
    raw := json.RawMessage(`{"path":` + jsonString(file) + `}`)

    res, err := tool.Call(context.Background(), raw, nil)
    if err != nil {
        t.Fatal(err)
    }
    got := res.StructuredContent.(fileStatResult)
    if got.Size != 5 || got.IsDir {
        t.Fatalf("result = %+v", got)
    }
}
```

至少覆盖：

- 正常路径
- 越界路径（`../`、root 外绝对路径）
- 不存在路径
- 非法参数/未知字段
- 取消 context
- 输出上限

### 4. 重启地端并验证

```bash
go test ./...
go run ./cmd/termio run --config /path/to/config.json
```

重启后地端会重新注册，网关会拿到新的 `tools/list`。验证：

```bash
curl -H "Authorization: Bearer <token>" \
  http://127.0.0.1:8787/v1/devices/<device_id>/tools
```

然后用 Postman 或 curl 调用：

```bash
curl -X POST http://127.0.0.1:8787/v1/devices/<device_id>/tools/call \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"name":"file_stat","arguments":{"path":"/absolute/path"}}'
```

### 5. 更新云端 Agent 的工具定义

地端注册了新工具，不代表云端 Agent 会自动把它暴露给模型。云端还需要一个同名 stub：

```python
def file_stat(path: str) -> dict:
    result = call_tool("file_stat", {"path": path})
    if result.get("isError"):
        raise RuntimeError(result["content"][0]["text"])
    return result.get("structuredContent", {})
```

如果云端支持动态工具发现，可以读取：

```http
GET /v1/devices/{device_id}/tools
```

把每个 `inputSchema` 映射成 Agent 框架的工具 schema。例如：

- Anthropic tool use：`{"name": ..., "description": ..., "input_schema": inputSchema}`
- OpenAI function calling：`{"type":"function","function":{"name":...,"description":...,"parameters":inputSchema}}`

### 6. 二次开发检查清单

- [ ] 参数用 `decodeArgs` 解析，拒绝未知字段
- [ ] 路径访问只通过 `Sandbox.Resolve` + `os.Root`
- [ ] 所有循环检查 `ctx.Done()`
- [ ] 结果有明确上限（条数、字节数、深度）
- [ ] 不启动 shell、不拼接命令字符串、不执行用户提供的命令
- [ ] 不把 token/密钥/敏感环境变量写进结果
- [ ] 工具失败返回 `error`（转成 `isError`），参数错误返回 `*ArgError`
- [ ] 输出可 JSON 序列化，`structuredContent` 使用导出字段
- [ ] 有单测覆盖越界、取消、上限
- [ ] 更新云端 stub 和 Agent 的 tool schema
- [ ] 更新 `docs/tools.md` 和 README 中的工具列表

### 关于写操作

v1 只提供只读工具。新增写文件、删除、shell、进程管理这类工具前，必须先设计：

- 能力声明与用户授权（哪些工具、哪些路径、哪些参数）
- 危险操作的二次确认
- 审计日志与可撤销性
- 幂等与失败恢复

不要直接把 `os/exec` 或 `os.Remove` 包装成工具——这会把“远程读文件”升级成“远程控制机器”，安全模型完全不同。

## 工具版本与兼容性

- 工具名是稳定契约；改名等同于新工具，云端需要同步更新。
- `inputSchema` 可以新增可选字段；删除/改名字段属于破坏性变更。
- 地端重启后网关会刷新工具列表；云端应容忍设备离线或工具列表变化。
- 如需按设备暴露不同工具，可在 `DefaultRegistry` 之外根据配置动态构建 registry。

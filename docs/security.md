# 安全模型

Termio 的本质是“让云端按需读取用户本机文件”。这是一个高权限能力，安全边界必须清楚。

## 威胁模型

### 资产

- 用户本机文件（通过 allowed roots 暴露的部分）
- 用户的设备 token
- 云端 Agent 的工具调用决策与结果
- 网关持有的设备会话

### 信任边界

- **云端 Agent / 网关**：被信任为按租户和权限发起调用。如果网关被攻破，攻击者可以调用已注册的只读工具。
- **地端端子**：被信任为执行策略（路径、工具白名单、并发、超时）。它不信任云端传来的路径和参数。
- **网络**：不可信。必须使用 `wss://`（回环地址除外）。
- **本机其他进程**：不可信。配置文件权限、token 存储、日志都可能被本机低权限进程读取。

### 主要攻击面

| 攻击面 | 缓解 |
|---|---|
| 路径穿越 / 符号链接逃逸 | 每工作区 `os.Root` + 绝对/相对路径校验 + 组件级包含检查 |
| 工具读取端子配置/令牌 | `$LARES/.termio` 加入沙箱 deny 列表；自定义 config/log 也 deny |
| 云端被攻破后调用危险工具 | 只读工具白名单，无 shell/写操作 |
| token 泄露 | 每设备独立 token、TLS、0600 存储、轮换（v1 未实现轮换） |
| 跨租户访问 | 按设备 token 鉴权；共享 token 仅开发用 |
| 本地日志泄露 | 不记录 token/Authorization/工具参数 |
| 重放/重复执行 | `callId` 幂等缓存；只读工具本身可重试 |
| 资源耗尽 | 并发上限、超时、结果条数/字节上限、搜索深度上限 |
| 本地提权 | 用户级自启动，不以 root/LocalSystem 运行 |

## 认证

### 地端 → 网关

```http
Authorization: Bearer <device-token>
X-Termio-Device: <device_id>
```

网关在 WebSocket 升级前验证 token 与 device_id 的绑定。

- **推荐**：`--devices-file` 为每台设备配置独立 token。
- **仅开发**：`--token` 共享令牌可授权任意设备；多租户生产不要使用。
- 同时配置时，共享令牌对所有设备有效，按设备 token 对本设备有效。

### 云端 → 网关

同样的 Bearer token。生产环境应使用**独立的云端 API token**，不要复用设备 token；参考网关为了简化，两者共用同一套 token。

### 传输

- 生产必须 `wss://`。
- 配置层拒绝非回环的 `ws://`。
- 参考网关本身只提供 HTTP/WS；生产需在 Caddy/Nginx 等反向代理上终止 TLS。
- 反向代理的 idle timeout 必须大于心跳间隔（默认 20s），否则长连接会被切断。

### Token 存储

- Unix 配置文件权限 `0600`。
- Windows 依赖用户目录 ACL；Go 的 `Chmod` 不会设置 ACL。
- `%AppData%` 可能被企业策略或 OneDrive 漫游同步，token 可能复制到其他机器；敏感场景用 `--config` 指向 `%LocalAppData%`。
- v1 没有 token 轮换、吊销、设备配对。生产需要补齐。

## 授权与工具面

- 地端只注册 `search_files`、`read_file`、`system_info`，都是只读。
- 没有 shell、PTY、写文件、删除、进程管理、网络工具。
- 工具名和参数由地端校验；云端无法调用未注册的工具。
- 每台设备为每个已注册工作区打开独立 `os.Root`；不同租户的文件在物理上由不同地端进程隔离。
- 工作区由本地用户注册，默认 `$LARES`（`~/.lares`）；云端只能在已注册 ID 中选择，不能新增路径，也不能通过工具参数指定文件系统路径。注册工作区等于授权 Agent 访问该目录，见 [workspace.md](workspace.md)。
- `session_id` 只是云端会话标签，**不是鉴权主体或隔离边界**；同一设备的所有会话共享该设备 token 的权限。需要跨租户隔离时必须使用不同设备/token。
- 每次调用只进入一个工作区；工作区之间不能通过相对路径或绝对路径串目录。
- 端子信息（config、token、日志）在 `$LARES/.termio`，对**所有**工作区 deny；自定义 config/log 位于任何工作区时同样 deny。
- 云端 Agent 的工具 stub 应只暴露业务需要的工具；不要把整个 registry 无差别地交给模型。

## 沙箱

实现位于 `internal/tools/sandbox.go`：

- 每个工作区及其额外根都用 `os.OpenRoot` 打开；一个调用只会使用其中一个工作区的 sandbox/registry。
- 空路径/相对路径以该调用所选工作区为基准；相对路径的 `..` 不能逃出工作区；绝对路径必须在该工作区或其额外根内。
- `$LARES/.termio` 以及实际 config/log 路径是全局 deny 路径；任何工作区或额外根都不能设在 deny 路径内。
- 拒绝 UNC、Windows 设备路径、`C:foo`、NTFS ADS 和越界 `..`。
- 请求路径先规范化（符号链接、8.3 短名），再与 root 做组件级比较。
- `os.Root` 在打开时再次阻止符号链接/junction 逃逸，消除 check-then-open 的 TOCTOU。
- 搜索不跟随目录符号链接。
- 不展开请求路径里的环境变量或 `~`；`~` 只在配置初始化时展开。
- 硬链接是已知边界：能创建硬链接的攻击者必须已有 root 内写权限。v1 不额外检测 inode 归属。

### 残余风险

- 文件可能在检查后、读取前被替换；`os.Root` 保护的是路径解析，不保护文件内容一致性。
- 读取正在写入的文件可能得到不完整内容；`read_file` 会如实报告 `size` 和 `truncated`。
- macOS TCC、Windows Controlled Folder Access、企业 AV 可能在 `os.Root` 之外拒绝访问；工具会返回权限错误。
- 根目录本身如果是 `/`、盘符根或 home，暴露面很大；`init`/`workspace add`/`set` 会打印 warning，`doctor` 尚未实现，配置时应尽量收窄。
- 所有已注册工作区都对云端可见（ID/名称）；云端可以在其中任意选择。只注册确实愿意授权给 Agent 的目录。

## 操作系统控制

### macOS

- 需要给端子二进制授予 **Full Disk Access**，否则 Desktop/Documents/Downloads 等受 TCC 保护的目录会拒绝访问。
- 自启动使用 LaunchAgent（用户级），不是 LaunchDaemon。
- 正式分发需要 Developer ID 签名 + hardened runtime + notarization；否则 Gatekeeper 会拦截。

### Windows

- 不使用 Windows Service（LocalSystem），因为它读不到当前用户文件；使用计划任务（当前用户）或 HKCU Run。
- Defender Controlled Folder Access、企业 AV 可能拦截文件访问或出站连接；发布版本应做 Authenticode 签名。
- 拒绝 UNC/设备路径和 ADS，减少 SMB/命名管道/备用数据流攻击面。

### Linux

- 以普通用户运行；不要用 root systemd unit。
- 开机无登录运行需要 `loginctl enable-linger $USER`。
- 检查 SELinux/AppArmor 是否会阻止读取 allowed roots。

## 日志与隐私

- 不记录 token、Authorization 头、工具参数或文件内容。
- 网关记录设备 ID、工具名、callId、HTTP 状态、耗时；debug 级别记录进度消息。
- 地端记录启动、注册、连接错误、工具名、callId；不记录路径参数。
- 日志文件默认在 `$LARES/.termio/termio.log`；生产环境应限制权限并配置轮转。

## 参考网关的生产化清单

当前 `termio-gateway` 是单实例参考实现，**不能直接作为公网多租户服务**。上线前至少需要：

- [ ] 独立云端身份体系（API key/OAuth/mTLS），与设备 token 分离
- [ ] 设备配对/绑定、token 轮换与吊销、哈希存储
- [ ] 租户/设备数据库、权限模型、审计日志
- [ ] 速率限制、并发配额、异常检测
- [ ] TLS 终止、WAF、反向代理超时调优
- [ ] 多实例网关 + presence + 内部总线（NATS）
- [ ] 指标、追踪、告警
- [ ] 设备 token 泄露的应急流程
- [ ] 代码签名、公证、自动更新签名校验
- [ ] 对 `4003`（重复连接）、`32010`（过载）、`401` 的监控

## 应急响应

如果怀疑设备 token 泄露：

1. 在网关侧吊销/替换该设备 token（生产系统），或停用共享 token。
2. 在地端停止服务：`termio stop`，或直接结束进程。
3. 检查地端与网关日志中的异常 callId、工具调用频率、来源 IP。
4. 缩小 `workspaces`（删除不必要的工作区或额外根），重新生成设备 ID 与 token。
5. 如涉及文件泄露，按租户的数据安全流程处理。

## 已知限制

- 无端到端加密：网关能看到工具参数和结果。若 relay 不可信，需要在云端 Agent 与地端之间增加应用层加密。
- 无设备配对/OAuth、mTLS、token 轮换。
- 无写操作审批、能力声明、审计数据库。
- 无持久化幂等日志；地端重启后重试可能重复执行。
- 只读工具，但 `read_file` 仍可能读取敏感文件；所有工作区/额外 roots 必须按最小权限配置。
- `session_id` 不提供额外权限隔离；网关按设备 token 鉴权。

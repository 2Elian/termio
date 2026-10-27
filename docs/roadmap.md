# 路线图

这是一份**优先顺序**，不是承诺的日期。原则：

- 先保证协议和沙箱正确，再扩展能力。
- 先生产化（认证、多实例、可观测），再增加危险工具。
- 不为了“用 MQ”而用 MQ；只有出现明确需求才引入。
- 地端协议尽量稳定；新增能力优先放在 `_meta`、工具或网关层。
- 一个用户/设备一个端子；云 Agent worker 尽量无状态；连接状态归 Gateway。

## V1（当前）

已交付：

- Go 单二进制地端端子，Windows / macOS / Linux（amd64/arm64）
- 反向 WebSocket + JSON-RPC 2.0 + MCP 工具形状
- `search_files` / `read_file` / `system_info`，只读
- 工作区：默认 `~/.lares`，用户本地切换；工具默认作用于工作区，支持相对路径；端子信息在 `~/.lares/.termio` 且对工具 deny
- `os.Root` 沙箱、绝对/相对路径、符号链接/junction 逃逸拦截
- 心跳、重连、4003 取代冷却、并发限制、相对超时、取消、进度
- `callId` 幂等缓存（10 分钟 / 1024 条）
- 用户级自启动（systemd user / LaunchAgent / 计划任务）
- 单实例参考网关 + HTTP API + E2E 测试
- VS Code 调试配置、Postman 集合

已知边界：

- 网关单实例、内存态、静态 token；无 HA、限流、审计
- 无设备配对、token 轮换/吊销、mTLS、端到端加密
- 无离线队列；地端重启丢失幂等缓存
- progress 只写网关日志，不流式返回云端
- 无桌面 UI/托盘、自动更新、代码签名/notarization
- 只读工具；没有写操作审批模型

## V1.x：把“能用”变成“可分发”

目标：真实用户能安全安装、升级、排障，不改变核心协议。

1. **真实机器验证**
   - macOS Intel/Apple Silicon、主流 Linux 发行版的安装/自启动/文件权限
   - Windows 非管理员安装、Defender/AV 误报、长路径
2. **`termio doctor`**
   - 配置、workspace/额外 roots 可读性、网关连通性、TLS、token 有效性
   - macOS Full Disk Access、Windows Controlled Folder Access 提示
3. **日志与诊断**
   - 日志轮转、诊断包导出（脱敏）
   - `termio status --json`、最近的连接/错误摘要
4. **发布工程**
   - Windows Authenticode 签名
   - macOS Developer ID + hardened runtime + notarization
   - 自动更新（签名 manifest + 回滚）
   - 安装包/卸载脚本
5. **质量**
   - Linux CI 跑 `-race`
   - 更多沙箱/协议回归测试；故障注入（断网、半开连接、磁盘满、权限变化）
6. **最小本地 UI/托盘（面向普通用户时提前）**
   - 配对、workspace/额外 roots、暂停/恢复、状态、日志入口
   - 选型见 [architecture.md](architecture.md) 的“未来 UI / 托盘”：Tauri / Wails / Electron
   - 只通过本地控制面访问 daemon，不暴露公网
   - 如果早期用户是开发者，可以先用 CLI；如果直接面向普通用户，不要把 UI 拖到 V2

## V2：生产级多租户

目标：一个云端 Agent 服务安全地服务大量用户的端子；Gateway 可水平扩展。

### V2.1 身份与安全

- 设备配对/绑定：一次性配对码、设备码或 OAuth device flow
- 每设备独立凭证；token 轮换、吊销、哈希存储
- 云端 API 身份与设备 token 分离（API key/OAuth/mTLS）
- 租户/设备授权模型；跨租户访问测试
- 速率限制、并发配额、异常检测
- 审计日志（谁、何时、对哪台设备调用了什么工具）

### V2.2 分布式 Gateway

- 拆分 Cloud Agent worker（无状态）与 Gateway（有状态连接面）
- presence：`device_id → gateway_instance_id`，带 TTL 租约
- 内部总线：NATS subject-per-device 或实例寻址 RPC；进度/结果通过 reply inbox
- 多实例部署、会话 fencing、滚动升级、断线重连
- 明确多实例下的重复连接策略（last-wins + 4003）
- 参考网关保留为本地开发实现；生产网关可以是独立服务

### V2.3 可观测性与运维

- OpenTelemetry metrics/tracing；按设备/租户/工具维度
- 连接数、调用延迟、错误率、过载、重连、超时面板
- 告警：401、4003、32010、32011、长时间无心跳
- 审计导出、SIEM 集成
- 容量规划：单 Gateway 连接数、内存、文件描述符

### V2.4 交付语义

- progress 流式返回云端：SSE 或独立的 WS/HTTP streaming 端点
- 持久化幂等状态：Redis/DB 保存短 TTL 的调用结果/状态，跨 Gateway/地端重启
- 离线策略二选一：
  - 明确不排队（保持 V1 语义），或
  - 可选持久队列（NATS JetStream / MQTT 5），带 TTL、配额和“设备离线”提示
- 重试语义文档化：at-least-once、幂等窗口、何时安全重试

### V2.5 工具治理

- 每租户/设备/工具的策略（允许、禁止、参数限制）
- 工具 schema 版本化、能力协商、兼容性检查
- 工具调用配额与超时策略
- 工具列表变更通知/缓存刷新

### V2.6 本地 UI / 托盘

- 配对、workspace/额外 roots 管理、暂停/恢复、状态、日志、审批
- UI 是本地控制面的客户端：Unix socket / Windows named pipe（或仅回环 + 本地鉴权）
- 选型见 [architecture.md](architecture.md) 的“未来 UI / 托盘”一节：Tauri / Wails / Electron
- 协议不变；UI 不能绕过沙箱和策略

### V2 验收标准（建议）

- 跨租户访问测试为零通过
- Gateway 多实例滚动升级不丢已注册设备（重连窗口内自动恢复）
- token 轮换/吊销可在线完成
- 每设备/每租户的并发与速率限制可配置
- 指标/审计能定位一次完整调用链
- 明确并测试离线/重试语义

## V3：平台能力

目标：从“只读工具通道”扩展为可控的本地能力平台。V3 的前置条件是 V2 的身份、审计和策略。

### V3.1 写操作与危险工具

- 工具能力声明（读/写/执行/网络）
- 审批模型：每次调用、会话级、长期策略；本地 UI 确认
- 参数级约束、路径白名单、命令白名单（如果将来真的需要 shell）
- 审计、可撤销操作、失败恢复
- 默认关闭；租户显式开启

### V3.2 策略与数据保护

- 策略引擎：按租户/设备/工具/参数/时间
- 敏感路径/内容脱敏；DLP 集成
- 配额、预算、速率限制
- 多级审批与职责分离

### V3.3 端到端加密

- Cloud Agent ↔ Daemon 应用层加密，Gateway 只转发密文
- 密钥分发/轮换；设备撤销
- 与审计/可观测的平衡（元数据仍可见）

### V3.4 工具生态 / 插件

- 独立进程工具（子进程/RPC）与资源隔离
- WASM 或受限运行时（视需求）
- 签名插件、版本管理、权限声明
- 开发者 SDK 和测试工具链
- 工具市场/分发（如果产品需要）

### V3.5 多设备与接力

- 一个用户多台设备：设备选择、在线状态、能力差异
- 会话/任务在设备间接力（需要明确状态归属）
- 设备分组/租户级策略

### V3.6 多区域与高可用

- 多区域 Gateway、就近接入
- 灾难恢复、数据驻留
- 公平调度/隔离吵闹邻居

### V3.7 完整 MCP 兼容（按需）

- 如果生态明确需要：Streamable HTTP、resources、prompts、sampling
- 保留反向 WebSocket 作为设备通道；MCP 适配放在 Gateway/云端
- 不为了“兼容”而牺牲 NAT 穿透和取消/进度语义

## 传输与中间件演进决策

| 需求 | 选择 |
|---|---|
| 设备交互通道 | 继续 WebSocket；不要换成 MQ |
| 多 Gateway 内部路由 | NATS（presence + subject-per-device / 实例寻址 RPC） |
| 进度流式返回云端 | SSE 或独立 streaming 端点；不要让 HTTP 请求无限长挂 |
| 地端离线任务不能丢 | MQTT 5 持久会话或 NATS JetStream；明确 TTL/配额/语义 |
| 大规模弱网设备 | 评估 MQTT 5；可作为 WS 的补充或替代通道 |
| 持久幂等 | Redis/DB 短 TTL；不要假装 exactly-once |

## 明确不做（可预见范围）

- 把端子做成公网可访问的远程 shell/远程桌面
- 没有审批/审计的写操作
- 用 Kafka/RabbitMQ 作为设备通道
- 让 Cloud Agent worker 自己持有全部地端长连接
- 让 UI 绕过 daemon 的沙箱和策略

## 如何决定下一步

- 目标用户是普通用户而不是开发者 → 把最小 UI/托盘和配对提前到 V1.x
- 用户装不上/不敢装 → V1.x 签名、安装包、UI
- 云端设备量上来 → V2.2 分布式 Gateway + V2.3 可观测
- 企业客户要合规 → V2.1 身份/审计 + V3.2 策略
- 业务需要“改文件/执行命令” → V3.1，且必须先有 V2 的审计与审批
- 需要跨地域 → V3.6，不要过早做

# Echoa 迁移到 MCP 2026-07-28 无状态 Streamable HTTP

状态：已完成（已合并至 main 分支）。规范依据是 [MCP 2026-07-28 规范](https://modelcontextprotocol.io/specification/2026-07-28/changelog)，核心采纳已定稿的 [SEP-2575](https://modelcontextprotocol.io/seps/2575-stateless-mcp)（移除握手与生命周期绑定）和 [SEP-2567](https://modelcontextprotocol.io/seps/2567-sessionless-mcp)（移除协议级 Session）。

## 结论

Echoa 最初实现的是 2024-11-05 的 HTTP+SSE（`GET <endpoint>/sse` 开长连接，首个事件下发 `POST <endpoint>/message?sessionId=`，响应从 SSE 通道回去）。该传输方式在 2026-07-28 规范中被标为 Deprecated。在当前版本中，Echoa 已完成向 2026-07-28 无状态 Streamable HTTP 的重构。

工具层保持不变：MySQL 和 Redis 的工具均属于单次请求、只读且无状态的操作。连接池按数据源复用，执行主体来源于每次请求的 Casdoor JWT。Redis 的 `SCAN` 游标在单次调用内消化，不跨请求维持上下文。重构范围聚焦于传输通道、协议封装与并发治理。

Echoa 采用双代兼容（Dual-Era）架构：对 Cursor、Claude Desktop、Cline 等主流客户端，支持标准的 `initialize` 握手（自动协商版本 `2024-11-05` ~ `2025-11-25`）、`notifications/initialized`、`ping` 以及标准 JSON-RPC 的工具调用，无需自定义标头；同时完整支持 2026-07-28 无状态 Streamable HTTP 规范与 `server/discover`。在传输层彻底移除了长连接与会话状态管理，服务完全无状态化，极大降低了运维复杂度与内存开销。

## 架构对比

| 维度 | 迁移前 (2024-11-05 SSE) | 迁移后 (2026-07-28 无状态) |
| :--- | :--- | :--- |
| 端点 | `GET <path>/sse`，`POST <path>/message?sessionId=` | 单一 `POST <path>` |
| 握手 | `initialize` + `notifications/initialized` | 无握手。每个请求的标头与 `_meta` 自带协议版本和方法 |
| 会话 | 内存 Session，15 分钟空闲回收，每 Session 5 并发 | 无协议级 Session。进程内限流按「endpoint + 鉴权主体」 |
| 响应 | POST 返回 202，结果走 SSE | POST 直接返回 `application/json` (HTTP 200) |
| 发现 | 仅依赖 `initialize` 响应 | 标准 `server/discover` 接口 |
| 鉴权 | 每次请求验 JWT（无状态） | 不变（Casdoor JWT / RFC 9728 PRM） |

路由在 [cmd/server/main.go](../cmd/server/main.go)，按 `endpoints` 逐条挂载。每个挂载路径对应一个独立的 MCP 服务实例（[internal/mcp/server.go](../internal/mcp/server.go)），工具表互不可见，入口为单一 POST。默认配置下是 `POST /mysql` 和 `POST /redis`；新增 MySQL 数据源时可挂载为 `POST /mysql2`。

`path` 约束保持不变：必须为单段路径，且不能占用 `/health` 或以 `/.` 开头的路径（由 [internal/config/config.go](../internal/config/config.go) 的 `Validate` 校验）。

## 请求处理与协议规则

客户端向 `<path>` 发起 POST 请求。服务端按以下步骤进行校验与分发：

1. **鉴权**：`CasdoorMiddleware` 拦截，缺少 Token 时返回 401 及 `WWW-Authenticate: Bearer resource_metadata="..."`；校验失败返回 401 及 `error="invalid_token"`。
2. **DNS Rebinding 防护**：校验 `Origin` 请求头。若头存在且不在 `server.allowed_origins` 列表（默认仅放行本地回环及空 Origin），返回 HTTP 403。
3. **标头一致性强校验**：
   - 请求必须携带 `MCP-Protocol-Version: 2026-07-28`，且与请求体 `params._meta["io.modelcontextprotocol/protocolVersion"]` 一致；若缺失或不匹配返回 400（错误码 `-32020 HeaderMismatch`）。
   - 若协议版本不属于支持列表，返回 400（错误码 `-32022 UnsupportedProtocolVersion`）。
   - 请求必须携带 `Mcp-Method` 请求头，且与 JSON-RPC `method` 一致；否则返回 400（`-32020`）。
   - 调用 `tools/call` 时，必须携带 `Mcp-Name` 请求头（支持 RFC 2047 MIME Base64 编码）且与 `params.name` 一致；否则返回 400（`-32020`）。
4. **通知处理**：对于未携带 `id` 的通知类请求（如 `notifications/initialized`），服务端直接返回 HTTP 202 Accepted，不执行业务逻辑。
5. **方法分发**：
   - 未知方法直接返回 HTTP 404 及错误码 `-32601 Method not found`。
   - 合法方法在当前 endpoint 的独立工具注册表中执行，成功返回 HTTP 200。
6. **非 POST 方法拦截**：`GET`、`DELETE` 等请求统一返回 HTTP 405 Method Not Allowed 并带有 `Allow: POST`。

### 支持的 RPC 方法

| 方法 | 行为说明 |
| :--- | :--- |
| `initialize` | 主流客户端协议握手与版本协商，返回 `capabilities.tools`、`serverInfo` 及协商后的 `protocolVersion`（如 `2024-11-05`）。 |
| `ping` | 连接活性探活，返回 `{}` 空结果。 |
| `server/discover` | 现代 2026-07-28 客户端免握手发现，返回 `supportedVersions: ["2026-07-28"]`、`capabilities: { tools: {} }`、`ttlMs: 3600000`、`cacheScope: "public"`。服务身份放置于结果的 `_meta["io.modelcontextprotocol/serverInfo"]`。 |
| `tools/list` | 列出当前 endpoint 独享的工具清单。工具按名称升序稳定排序。 |
| `tools/call` | 执行具体的工具调用，附带并发控制与审计记录。工具内部错误以 `isError: true` 正常返回 HTTP 200，不作为协议级错误。 |

## 架构精简与已移除组件

在无状态重构中，彻底移除了原 HTTP+SSE 相关的状态维持模块：
- **移除了协议级 Session 机制**：废弃原 `Session`、`SessionManager`、GC 轮询协程、通道分发及心跳维护逻辑。
- **引入并发限流器**：基于 [internal/mcp/limiter.go](../internal/mcp/limiter.go) 实现 `InflightLimiter`，按「endpoint + 鉴权主体」（JWT 用户名或本地开发身份）进行轻量级槽位控制，单个主体默认最多 5 并发，超限即返回 HTTP 429。
- **结构化审计**：在 [internal/mcp/server.go](../internal/mcp/server.go) 中记录包含时间戳、端点、请求 ID、执行用户、工具名、耗时及脱敏参数的结构化 JSON 日志。
- **超时机制划分**：HTTP 服务不设置固定的 `WriteTimeout`，避免阻断合法的大查询或分析任务；单次查询耗时严格由各自数据源的 `query_timeout_ms`（context deadline）保障。

## 配置与部署

在 [config/config.yaml](../config/config.yaml) 中维护相关参数：
- `server.allowed_origins`：跨域 Origin 白名单，默认为空（放行非浏览器客户端与 localhost）。
- `server.max_inflight_per_principal`：单主体最大并发执行数，默认 5。

在反向代理层面，由于所有 POST 请求自包含且无会话绑定，支持任意负载均衡（无需配置 Session Sticky）。代理配置需确保透传 `MCP-Protocol-Version`、`Mcp-Method`、`Mcp-Name` 及 `Authorization` 请求头。

## 测试覆盖

在 [internal/mcp/integration_test.go](../internal/mcp/integration_test.go) 中已全面覆盖无状态 HTTP 规范测试：
- `TestMCPMethods405`：验证 GET/DELETE 请求返回 405 及 Allow 头。
- `TestMCPOriginValidation`：验证合规与非法 Origin 的 403 阻断。
- `TestMCPHeaderValidation`：覆盖缺失版本头、版本与 body 不一致、版本不受支持（-32022）、缺失方法头、方法头不匹配等各类 400 场景。
- `TestMCPNotification`：验证通知类消息正确返回 202。
- `TestServerDiscover`：断言 `server/discover` 返回规范的完整字段与 serverInfo。
- `TestToolsList`：断言工具列表字段齐全且按字母升序稳定排序。
- `TestToolsCall`：覆盖成功调用、工具业务级报错（isError=true）以及敏感参数审计脱敏。
- `TestUnknownMethod404`：断言旧版 `initialize` 等未实现方法返回 HTTP 404 及 -32601。
- `TestInflightConcurrencyLimit429`：测试并发槽位耗尽后精准触发 HTTP 429。
- `TestEndpointIsolation`：测试多个 endpoint 之间数据源与工具集的深度隔离。

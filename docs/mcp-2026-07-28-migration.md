# Echoa 迁移到 MCP 2026-07-28 无状态 Streamable HTTP

状态：方案，不改代码。规范依据是 [2026-07-28](https://modelcontextprotocol.io/specification/2026-07-28/changelog)，主要来自已定稿的 [SEP-2575](https://modelcontextprotocol.io/seps/2575-stateless-mcp)（去掉握手）和 [SEP-2567](https://modelcontextprotocol.io/seps/2567-sessionless-mcp)（去掉协议级 session）。

## 结论

Echoa 现在实现的是 2024-11-05 的 HTTP+SSE：`GET <endpoint>/sse` 开长连接，首个事件下发 `POST <endpoint>/message?sessionId=`，响应从 SSE 通道回去。协议版本写死为 `2024-11-05`（[internal/mcp/dispatcher.go](../internal/mcp/dispatcher.go)）。这条传输从 2025-03-26 起已被 Streamable HTTP 取代，在 2026-07-28 里被标为 Deprecated。

工具层不用动。MySQL 和 Redis 的工具都是单次请求、只读、不挂在 session 上的状态：连接池按数据源复用，身份来自每次请求的 Casdoor JWT。Redis 的 `SCAN` 游标在一次工具调用内部消耗，不跨请求。迁移范围是传输和协议外壳，不是数据源模块。

建议只实现 2026-07-28，不做双代兼容。现有客户端（还在走 `initialize` + SSE 长连接的 Cursor / Claude 版本）会断开，直到它们自己升到这一代。这是有意的：规范把两代并存当成可选，但同时维护两套传输会把现在的 session 机器留下来，迁移就没有收益。

## 现状与目标

| | 现在 | 目标 |
| :--- | :--- | :--- |
| 端点 | `GET <path>/sse`，`POST <path>/message?sessionId=` | 单一 `POST <path>` |
| 握手 | `initialize` + `notifications/initialized` | 没有。每个请求的 `_meta` 自带版本和能力 |
| 会话 | 内存 session，15 分钟空闲回收，每 session 5 并发 | 没有协议级 session。限流按身份 |
| 响应 | POST 返回 202，结果走 SSE | POST 直接返回 `application/json`；需要流式时才用请求级 SSE |
| 发现 | 只有 `initialize` 的结果 | 必须实现 `server/discover` |
| 鉴权 | 每次请求验 JWT（已经是无状态） | 不变 |

路由在 [cmd/server/main.go](../cmd/server/main.go)，按 `endpoints` 逐条挂载。这个划分保留：每个挂载路径仍然是一个独立的 MCP 服务，工具表互不可见，只是入口从两条路由收成一条 POST。默认配置下是 `POST /mysql` 和 `POST /redis`；加了第二个 MySQL 后就是 `POST /mysql2`，而不是再挂 `/mysql2/sse`。

`path` 的约束不变：单段路径，不能占用 `/health` 或以 `/.` 开头的路径（[internal/config/config.go](../internal/config/config.go) 的 `Validate`）。收成一条 POST 之后，这条约束更重要：以前 `/message` 是固定后缀，现在 MCP 入口就是 `path` 本身。

## 目标形状

客户端对 `<path>` 发 POST。服务端按这个顺序处理，任一步失败都直接写 HTTP 响应，不再经过 session channel：

1. 鉴权。现有 `CasdoorMiddleware` 不动：缺 token 返回 401 和 `WWW-Authenticate`，JWT 验签失败同样。声明仍然放进请求 context。
2. `Origin`。头存在且不在允许列表时返回 403。本地默认只允许空 `Origin` 或 `http://127.0.0.1` / `http://localhost`；生产用配置补允许的来源。这是规范的 MUST，现在没做。
3. 头校验。`MCP-Protocol-Version` 必须是 `2026-07-28`，并且和 body 里 `_meta["io.modelcontextprotocol/protocolVersion"]` 一致。`Mcp-Method` 必须等于 JSON-RPC 的 `method`。`tools/call` 还要求 `Mcp-Name` 等于 `params.name`。不一致或缺头返回 400，JSON-RPC 错误码 `-32020`（`HeaderMismatch`）。版本不支持返回 400 和 `-32022`，`data.supported` 只列 `2026-07-28`。
4. 方法分发。未知方法返回 HTTP 404 加 JSON-RPC `-32601`。这和现在的做法不同：现在未知方法是走 SSE 回一个错误对象。
5. 执行并写回响应。默认 `Content-Type: application/json`，HTTP 200。通知（没有 `id`）接受则 202 空 body，拒绝则 4xx。

`GET` 和 `DELETE` 这个路径返回 405。带来的 `Mcp-Session-Id` 和 `Last-Event-ID` 忽略，不回显。

### 支持的方法

只保留 Echoa 真正用到的能力，加上规范强制的发现：

| 方法 | 行为 |
| :--- | :--- |
| `server/discover` | 返回 `supportedVersions: ["2026-07-28"]`、`capabilities: { tools: {} }`、可选 `instructions`。服务身份放结果的 `_meta["io.modelcontextprotocol/serverInfo"]`，不再是顶层字段。 |
| `tools/list` | 该 endpoint 自己的工具表，看不到别的 endpoint 引用的 source。结果加 `resultType: "complete"`、`ttlMs`、`cacheScope: "public"`。工具集在进程启动时固定，建议 `ttlMs` 取一小时。列表顺序改为按名字排序，现在 map 遍历顺序不确定，规范要求 SHOULD 确定。 |
| `tools/call` | 现有执行路径。结果加 `resultType: "complete"`。不实现 MRTR：工具不需要向客户端要额外输入。 |

不实现，直接当未知方法：`initialize`、`ping`、`logging/setLevel`、`resources/*`、`prompts/*`、`subscriptions/listen`。Echoa 没有会变的列表，也没有要推送的变更，`capabilities.tools` 不带 `listChanged`。客户端靠 `ttlMs` 判断工具表要不要重拉。

`notifications/initialized` 之类没有 `id` 的通知：核心协议在这一代的 HTTP 上没有客户端通知。收到则返回 202，不执行任何东西。取消是客户端关掉响应流，不是发 `notifications/cancelled`。

### 响应示例

`server/discover`：

```json
{
  "jsonrpc": "2.0",
  "id": "discover-1",
  "result": {
    "resultType": "complete",
    "supportedVersions": ["2026-07-28"],
    "capabilities": { "tools": {} },
    "instructions": "Read-only tools for the sources mounted on this endpoint. Every call is independent and requires its own arguments.",
    "ttlMs": 3600000,
    "cacheScope": "public",
    "_meta": {
      "io.modelcontextprotocol/serverInfo": { "name": "echoa", "version": "1.0.0" }
    }
  }
}
```

`tools/call` 的结果在现有 `content` 上加 `resultType`。工具执行失败仍然是成功的 JSON-RPC 响应加 `isError: true`，不是 JSON-RPC error，这点保持现状。

## 什么可以删

协议级 session 整套退出：[internal/mcp/session.go](../internal/mcp/session.go) 的 `Session`、`SessionManager`、空闲 GC、发送 channel 和 5 秒发送超时；[internal/mcp/handler_sse.go](../internal/mcp/handler_sse.go) 的 endpoint 事件、`messagePath` / `resolveEndpointURL` 和 25 秒心跳；[internal/mcp/handler_message.go](../internal/mcp/handler_message.go) 的 `sessionId` 查询参数和 202 异步派发。`Server.sessionMgr` 和 `mountPath` 也一起去掉。`mountPath` 和 `X-Forwarded-Prefix` 拼接只为给 SSE 客户端下发 `/<path>/message?sessionId=` 而存在；客户端直接配置 `POST <path>` 之后，服务端不再回调地址，代理前缀由代理自己剥掉。

并发保护不能随之消失，只是换键。现在的信号量挂在 session 上，而新协议没有 session。改成按「endpoint + 鉴权主体」限流：鉴权开启时用 JWT 的 subject（落到 Casdoor claims 的稳定用户标识），鉴权关闭时用固定的 `developer` 身份。键里带上 endpoint，因为每个 endpoint 是一个独立服务，对 `/mysql` 的并发不应该占用 `/redis` 的名额。每个键同时最多 5 个正在执行的工具调用，超出返回 HTTP 429，语义和现在一致。空闲键的计数器定期清掉，避免无限增长。这个计数器是服务器自己的保护，不是协议状态，不跨请求影响结果。

审计日志里的 `session_id` 换成请求的 JSON-RPC `id` 加上用户名。脱敏规则不变。

`WriteTimeout` 仍然不设。单次响应不再是长连接，但一条慢查询可能超过任何固定的写超时；查询自身的 `query_timeout_ms` 已经约束执行时间。

## 配置

[config/config.yaml](../config/config.yaml) 加两项，都有默认值：

- `server.allowed_origins`：允许的 `Origin` 列表。空则只放行没有 `Origin` 的请求（非浏览器客户端）和本地回环。
- `server.max_inflight_per_principal`：默认 5，替代原来的每 session 并发上限。

`auth.resource_url` 保留，它服务于 RFC 9728 元数据，不再参与拼接消息回调地址。

## 代理与部署

反向代理不再需要把一个客户端粘到某个实例。每个 POST 自包含，任意副本都能回答，前提是它们挂着同一份数据源配置。限流计数是进程内的，多副本时每个进程各计各的，上限会放大到「副本数 × 5」。这对只读查询可以接受；真要全局上限再单独做，不进这次迁移。

代理要放行 `MCP-Protocol-Version`、`Mcp-Method`、`Mcp-Name`、`Authorization`，并且不要缓存 POST 响应。工具列表的缓存是客户端按 `ttlMs` 做的，不是 HTTP 缓存。

## 文档与测试

[README.md](../README.md) 要改写的地方：端点速查表里的 `<endpoint>/sse` 和 `<endpoint>/message`，「按 endpoint 隔离」那条里关于 session 不跨 endpoint 共享的描述，安全分层里的「MCP Session 治理」，以及客户端配置示例里的 `type: sse` 和 `.../mysql/sse`。入口变成每个 endpoint 一个 POST，安全分层里的 session 改成按「endpoint + 身份」的并发上限。工具名规则（单 source 用 `mysql_query`，同 endpoint 多 source 用 `orders_mysql_query`）和 source 命名约束不用改。

测试覆盖这几条，替掉现在绑在 SSE 上的集成测试（含 `resolveEndpointURL` 的前缀拼接用例）：

- 缺头、头与 body 不一致、不支持的协议版本，各自的 HTTP 状态和错误码。
- 非法 `Origin` 返回 403；空 `Origin` 放行。
- `server/discover` 和 `tools/list` 的字段齐全，工具顺序稳定。
- `tools/call` 在鉴权开启和关闭时都能跑通，审计日志仍然脱敏。
- 同一主体对同一 endpoint 的第 6 个并发调用返回 429；同一主体打另一个 endpoint，或同一 endpoint 上的不同主体，互不影响。
- 两个 endpoint 各引用一个 MySQL 时，`POST /mysql` 的 `tools/list` 看不到 `/mysql2` 的工具；同一个 endpoint 挂两个 MySQL 时，工具名带 source 名字。
- `GET` 返回 405；带 `Mcp-Session-Id` 的 POST 仍按正常请求处理。

## 不做的事

- 不保留 `<path>/sse` 和 `<path>/message` 作为兼容层。
- 不实现 `subscriptions/listen`、进度通知、日志流。没有调用方，而且一旦加上就要重新引入长连接。
- 不引入显式 state handle。没有工具需要跨调用保持状态。
- 不改 MySQL / Redis 的校验、截断和连接池。
- 不做 stdio 传输。Echoa 是独立的 HTTP 服务。

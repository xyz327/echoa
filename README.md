# Echoa

基于 **Golang + CloudWeGo Hertz + `hertz-contrib/sse`** 构建的企业级只读数据库 Model Context Protocol (MCP) 服务端。内置 **Casdoor OAuth 2.1 鉴权** 与 **RFC 9728 保护资源元数据发现**，为 AI Agent（如 Claude Desktop、Cursor、Cline、LibreChat 等）提供安全受控、深度隔离的 **MySQL** 与 **Redis** 只读分析能力。

参考规范与标准：
- [MCP Specification (SSE Transport)](https://modelcontextprotocol.io/)
- [RFC 9728: OAuth 2.0 Protected Resource Metadata](https://datatracker.ietf.org/doc/html/rfc9728)
- [googleapis/mcp-toolbox](https://github.com/googleapis/mcp-toolbox)
- [t8y2/dbx](https://github.com/t8y2/dbx)

---

## 目录

- [核心架构特性](#核心架构特性)
- [MCP 工具清单与参数定义](#mcp-工具清单与参数定义)
  - [MySQL 工具](#1-mysql-工具)
  - [Redis 工具](#2-redis-工具)
- [HTTP / SSE 端点速查](#http--sse-端点速查)
- [多层安全防护体系](#多层安全防护体系)
- [配置说明与环境变量](#配置说明与环境变量)
- [快速开始](#快速开始)
  - [本地原生运行](#1-本地原生运行)
  - [Docker Compose 一键全套联调（推荐）](#2-docker-compose-一键全套联调推荐)
  - [单容器运行](#3-单容器运行)
  - [发布与构建镜像](#4-发布与构建镜像)
- [客户端接入配置](#客户端接入配置)
  - [Claude Desktop](#1-claude-desktop)
  - [Cursor](#2-cursor)
  - [Cline / Roo Code (VS Code 插件)](#3-cline--roo-code-vs-code-插件)
- [代码目录结构](#代码目录结构)

---

## 核心架构特性

1. **高性能流式通信**：基于 CloudWeGo Hertz 原生网络栈与 `hertz-contrib/sse`，直接利用连接劫持（`HijackWriter`）与分块流式输出（`Chunked Writer`），极低内存占用与高并发连接支撑。
2. **标准 Casdoor OAuth 2.1 鉴权**：
   - 严格遵循 RFC 6750，通过 `Authorization: Bearer <jwt>` 标头进行无状态公钥验签。
   - 兼容老旧前端 EventSource 的 URL Query（`?token=...`）模式（已标记为 Deprecated 并记录安全告警）。
   - 实现 RFC 9728 规范的 Protected Resource Metadata (`/.well-known/oauth-protected-resource`)，支持客户端自动发现认证服务器与 Scope。
3. **MySQL 深度 AST 防御体系**：
   - 基于 `pingcap/tidb/pkg/parser` 对传入 SQL 做抽象语法树（AST）强校验，仅允许 `SELECT`、`EXPLAIN`、`SHOW`。
   - 彻底拦截 `SELECT ... FOR UPDATE`、`LOCK IN SHARE MODE` 等加锁读取，以及 `SELECT ... INTO OUTFILE / DUMPFILE` 等服务端写盘提权语句。
   - 智能 LIMIT 注入与改写：无 LIMIT 自动追加系统上限；超过配置上限强制压低改写；保留原分页 offset；子查询与外层查询严格隔离改写。
   - 敏感数据表黑名单过滤（支持 `table` 与 `schema.table` 精准匹配拦截）。
   - 双重防撑爆防护：单单元格 4KB 缩略截断 + 结果集 64KB 字符级硬上限。
4. **Redis 多 DB 独立隔离与连接池治理**：
   - 动态客户端管理器（`MultiDBManager`），每个逻辑 DB 独立连接池懒加载，**杜绝并发执行 `SELECT` 导致的数据串库重大隐患**。
   - 完备连接池生命周期参数（`pool_size`、`min_idle_conns`、10m 空闲超时、1h 存活淘汰），防御云原生网络下的静默断连。
   - 统一命令网关 `redis_execute`，内置命令白名单与硬黑名单防御（阻断 `KEYS`、`FLUSHALL`、`CONFIG`、写操作及手动切库 `SELECT`）。
   - 多类型（`[]string`、`map[string]string`、RESP3 `map[any]any`）双重截断保护（条目上限 + 64KB 响应硬上限）。
5. **传输韧性与可观测性**：
   - 单 Session 并发限制（默认最多 5 并发）与 HTTP 429 背压，后台空闲 15 分钟会话自动回收。
   - SSE 发送通道 5 秒超时熔断防线，杜绝慢客户端或死连接拖垮后端工作协程。
   - 结构化 JSON 审计日志（自动对 `password`、`token`、`secret` 等敏感参数脱敏，单参数超 200 字符缩略）。
   - K8s 存活（`/health`）与就绪（`/health/ready`）深度数据源探活探针。
   - 自动反代路由前缀兼容（通过 `X-Forwarded-Prefix` 或配置的 `resource_url` 自动拼装正确的 `/message` 回调端点）。

---

## MCP 工具清单与参数定义

服务端向 MCP 客户端暴露 6 个标准 Tool：

### 1. MySQL 工具

#### `mysql_list_tables`
列出当前数据库的所有可用表、类型、存储引擎、预估行数及表注释（自动过滤配置黑名单中的敏感表）。
- **参数**：无
- **返回示例**：
  ```json
  {
    "total_tables": 2,
    "tables": [
      {
        "name": "orders",
        "type": "BASE TABLE",
        "engine": "InnoDB",
        "rows_estimate": 4,
        "comment": "业务订单表"
      },
      {
        "name": "users",
        "type": "BASE TABLE",
        "engine": "InnoDB",
        "rows_estimate": 5,
        "comment": "用户基础表"
      }
    ]
  }
  ```

#### `mysql_describe_table`
查看指定表的结构定义，包含字段名、类型、是否允许 NULL、主键/索引键、默认值、额外属性及字段注释。
- **参数**：
  - `table_name` (`string`, 必填): 目标表名，支持 `users` 或 `test.users`。黑名单内的表会被直接拒绝。
- **返回示例**：
  ```json
  {
    "table": "users",
    "columns": [
      { "field": "id", "type": "bigint", "null": "NO", "key": "PRI", "default": "", "extra": "auto_increment", "comment": "" },
      { "field": "username", "type": "varchar(64)", "null": "NO", "key": "UNI", "default": "", "extra": "", "comment": "用户账号" },
      { "field": "email", "type": "varchar(128)", "null": "NO", "key": "", "default": "", "extra": "", "comment": "邮箱" }
    ]
  }
  ```

#### `mysql_query`
执行只读 SQL 查询。服务端进行 TiDB AST 审查，仅允许 `SELECT`、`EXPLAIN`、`SHOW`，并自动追加或改写 `LIMIT`。
- **参数**：
  - `query` (`string`, 必填): 待执行的 SQL 语句，例如 `SELECT id, username, email FROM users WHERE active = 1`。
  - `max_rows` (`integer`, 可选): 本次查询希望返回的行数上限（不能超过服务端配置的 `max_limit`）。
- **返回示例**：
  ```json
  {
    "query_executed": "SELECT id, username, email FROM users WHERE active = 1 LIMIT 50",
    "rows_returned": 2,
    "truncated": false,
    "execution_time_ms": 3,
    "rows": [
      { "id": 1, "username": "alice", "email": "alice@example.com" },
      { "id": 2, "username": "bob", "email": "bob@example.com" }
    ]
  }
  ```

---

### 2. Redis 工具

#### `redis_execute`
统一只读命令网关，支持常用读取命令（如 `GET`, `MGET`, `HGETALL`, `LRANGE`, `SMEMBERS`, `ZRANGE`, `TTL`, `TYPE` 等）。
- **参数**：
  - `command` (`string`, 必填): 命令名称（大小写不敏感），如 `"HGETALL"`、`"GET"`。
  - `args` (`array[string]`, 必填): 命令参数列表，例如 `["user:profile:1001"]`。
  - `db` (`integer`, 可选, 默认 `0`): 目标逻辑库索引（需在 `allowed_dbs` 允许列表内）。
- **注意**：
  - 严禁传入 `KEYS` 命令（请使用 `redis_scan_keys` 或 `SCAN`）。
  - 严禁执行 `SELECT` 命令（请通过 `db` 参数指定数据库）。
  - 严禁任何写入或高危管理命令（`SET`, `DEL`, `FLUSHALL`, `CONFIG`, `EVAL` 等）。

#### `redis_scan_keys`
使用游标 `SCAN` 安全遍历匹配 Glob 模式的 Key，分批迭代，杜绝生产环境阻塞。
- **参数**：
  - `pattern` (`string`, 必填): 匹配模式，如 `"user:*"`、`"*cache*"`。
  - `limit` (`integer`, 可选, 默认 `50`): 最大返回 Key 数量，受服务端上限压制。
  - `db` (`integer`, 可选, 默认 `0`): 目标逻辑库索引。
- **返回示例**：
  ```json
  {
    "db": 0,
    "pattern": "user:*",
    "count": 2,
    "truncated": false,
    "keys": [
      "user:profile:1001",
      "user:profile:1002"
    ]
  }
  ```

#### `redis_list_dbs`
通过安全读取 `INFO keyspace`，查看 Redis 实例各逻辑数据库（DB 0~15）中已存 Key 的数量与过期分布。
- **参数**：无

---

## HTTP / SSE 端点速查

| 路径 | 方法 | 鉴权要求 | 说明 |
| :--- | :--- | :--- | :--- |
| `/sse` | `GET` | 依配置（Bearer JWT） | MCP SSE 连接入口。连接建立后立即返回首个 `endpoint` 事件以及定期保活心跳 |
| `/message` | `POST` | 依配置（Bearer JWT） | MCP JSON-RPC 消息交互接口，通过 URL 参数 `sessionId` 绑定对应会话 |
| `/.well-known/oauth-protected-resource` | `GET` | 公开免鉴权 | **RFC 9728 PRM 元数据**，供 AI 客户端自动发现 OAuth 认证端点与 Scope |
| `/health` | `GET` | 公开免鉴权 | K8s 存活探针 (Liveness)，返回 HTTP 200 `{"status":"ok"}` |
| `/health/ready` | `GET` | 公开免鉴权 | K8s 就绪探针 (Readiness)，实际探测 MySQL 与 Redis 联通性，不健康时返回 HTTP 503 |
| `/health?type=ready` | `GET` | 公开免鉴权 | 就绪探针的 Query 参数兼容模式 |

---

## 多层安全防护体系

```
       AI Agent (Claude / Cursor / Cline)
                       │
             [ 1. Casdoor OAuth 2.1 ]
              Authorization: Bearer <JWT>
              PRM RFC 9728 自动发现 / 无状态验签
                       │
             [ 2. MCP Session 治理 ]
              并发限流 (5 并发/Session) + HTTP 429
              空闲 15 分钟会话超时回收 + 结构化脱敏审计
                       │
       ┌───────────────┴───────────────┐
       ▼                               ▼
[ 3. MySQL 只读防御 ]            [ 4. Redis 只读防御 ]
 ├─ TiDB AST 强语法校验           ├─ MultiDBManager 独立连接池
 ├─ 仅限 SELECT/EXPLAIN/SHOW      ├─ 杜绝并发 SELECT 串库漏洞
 ├─ 阻断 FOR UPDATE / 锁读         ├─ 强阻断 KEYS / CONFIG / FLUSH
 ├─ 阻断 INTO OUTFILE 写盘        ├─ 允许命令白名单机制
 ├─ 黑名单表拒绝访问               ├─ SCAN 游标安全分页防阻塞
 ├─ 强制智能注入 / 压低 LIMIT      └─ 双重截断保护:
 └─ 双重截断保护:                     ├─ 条目数截断 (max_items_limit)
     ├─ 单元格 4KB 截断              └─ 响应体 64KB 字符级截断
     └─ 结果集 64KB 硬上限
```

---

## 配置说明与环境变量

配置文件位于 `config/config.yaml`，原生支持 `${ENV_NAME:-default_value}` 语法注入：

```yaml
server:
  port: 8080                    # 服务监听端口
  read_timeout_ms: 15000        # HTTP 读取超时时间 (毫秒)

# Casdoor OAuth 2.1 鉴权配置
auth:
  enabled: false                # 本地开发测试置为 false (匿名调试)，生产环境必须设为 true
  resource_url: "${MCP_RESOURCE_URL:-http://127.0.0.1:8080}" # 服务对外公开 URL
  endpoint: "https://casdoor.example.com"                   # Casdoor 实例根地址
  client_id: "mcp-service"                                  # Casdoor 客户端 ID
  client_secret: "${CASDOOR_CLIENT_SECRET}"                 # 客户端密钥
  certificate: "${CASDOOR_JWT_CERT}"                       # Casdoor 应用的 x509 JWT 公钥证书内容
  organization_name: "your-org"                             # 组织名称
  application_name: "mcp-app"                               # 应用名称
  scopes_required:                                          # 要求的权限范围
    - "openid"
    - "offline_access"

# 只读数据源配置
datasources:
  mysql:
    enabled: true
    dsn: "${MYSQL_DSN:-root:password@tcp(127.0.0.1:3306)/test?charset=utf8mb4&parseTime=True&loc=Local&multiStatements=false}"
    max_open_conns: 20          # 连接池最大打开连接数
    max_idle_conns: 5           # 连接池最大空闲连接数
    query_timeout_ms: 5000      # 单次查询超时时间 (毫秒)
    security:
      max_limit: 200            # 单次查询强制限制的最大返回行数
      table_blacklist:          # 敏感表黑名单 (支持 "table" 或 "db.table")
        - "user_password"
        - "auth_tokens"
        - "sys_secret"

  redis:
    enabled: true
    addr: "${REDIS_ADDR:-127.0.0.1:6379}"
    password: "${REDIS_PASSWORD:-}"
    allowed_dbs: [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15] # 允许访问的 DB 列表
    pool_size: 20               # 每个 DB 连接池大小
    min_idle_conns: 5           # 最小空闲连接
    max_items_limit: 200        # 集合/列表类返回元素的最大上限
    query_timeout_ms: 3000      # 单次执行超时时间 (毫秒)
    security:
      allowed_commands:         # 允许执行的只读命令白名单
        - "GET"
        - "MGET"
        - "STRLEN"
        - "GETRANGE"
        - "HGET"
        - "HMGET"
        - "HGETALL"
        - "HKEYS"
        - "HVALS"
        - "HLEN"
        - "HEXISTS"
        - "HSCAN"
        - "LRANGE"
        - "LINDEX"
        - "LLEN"
        - "SMEMBERS"
        - "SISMEMBER"
        - "SCARD"
        - "SRANDMEMBER"
        - "SSCAN"
        - "ZRANGE"
        - "ZREVRANGE"
        - "ZRANGEBYSCORE"
        - "ZCARD"
        - "ZSCORE"
        - "ZCOUNT"
        - "ZSCAN"
        - "SCAN"
        - "TYPE"
        - "TTL"
        - "PTTL"
        - "EXISTS"
        - "PING"
        - "DBSIZE"
      blocked_commands:         # 明确禁止的高危只读命令
        - "KEYS"
        - "MONITOR"
        - "INFO"
```

### 关键环境变量对照表

| 环境变量 | 对应配置项 | 默认值 / 示例 |
| :--- | :--- | :--- |
| `MCP_RESOURCE_URL` | `auth.resource_url` | `http://127.0.0.1:8080` |
| `CASDOOR_CLIENT_SECRET` | `auth.client_secret` | - |
| `CASDOOR_JWT_CERT` | `auth.certificate` | Casdoor x509 证书内容 |
| `MYSQL_DSN` | `datasources.mysql.dsn` | `root:password@tcp(127.0.0.1:3306)/test?...` |
| `REDIS_ADDR` | `datasources.redis.addr` | `127.0.0.1:6379` |
| `REDIS_PASSWORD` | `datasources.redis.password` | 空 |

---

## 快速开始

### 1. 本地原生运行

要求环境：Go 1.22+（工程基于 Go 1.26 规范设计）。

```bash
# 1. 运行所有单元测试
make test

# 2. 编译服务端
make build

# 3. 指定配置文件启动
./bin/server -config config/config.yaml
```

### 2. Docker Compose 一键全套联调（推荐）

项目根目录包含完整的 `docker-compose.yml` 与预置数据库脚本 `scripts/init.sql`，一键拉起 MySQL 8.0、Redis 7.0 与 Echoa 服务：

```bash
# 一键构建并启动全套服务
make compose-up
# 等价于: docker compose up -d --build

# 检查健康检查探活状态 (Echoa 依赖 mysql 与 redis healthy 状态)
docker compose ps

# 查看 Echoa 服务端输出与 MCP 审计日志
docker compose logs -f echoa

# 停止并移除容器
make compose-down
```

测试就绪探针：
```bash
curl -i http://127.0.0.1:8080/health/ready
```

### 3. 单容器运行

```bash
# 构建轻量化 Alpine 镜像 (多阶段静态编译，无 CGO 依赖)
make image IMAGE=echoa TAG=latest

# 运行容器
docker run --rm -d -p 8080:8080 \
  --name echoa \
  -e MCP_RESOURCE_URL=http://localhost:8080 \
  -e MYSQL_DSN='root:password@tcp(host.docker.internal:3306)/test?charset=utf8mb4&parseTime=True&loc=Local&multiStatements=false' \
  -e REDIS_ADDR=host.docker.internal:6379 \
  echoa:latest
```

### 4. 发布与构建镜像

通过 [.github/workflows/image.yml](.github/workflows/image.yml) 实现 GitHub Actions 自动化多架构构建（`linux/amd64` 与 `linux/arm64`）：

- **GitHub Container Registry (ghcr.io)**：默认开启。推送到 `main` 分支会自动发布 `:latest` 与 `:sha-<短SHA>`；推送 `v*` tag 会自动打对应的版本号标签。
- **Docker Hub**：默认关闭。如需推送到 Docker Hub，在 GitHub 仓库添加 `DOCKERHUB_USERNAME` 与 `DOCKERHUB_TOKEN` (Access Token) Secrets，并将 workflow 中的 `env.dockerhub` 改为 `"true"`。

---

## 客户端接入配置

推荐使用 **OAuth 自动认证**：配置中只写服务 URL，不要预置 `Authorization`。客户端访问时收到 HTTP 401，从 `WWW-Authenticate` 中的 `resource_metadata` 发现 Casdoor，再打开浏览器完成登录。手工 Bearer Token 仅用于调试、自动化脚本，或不支持 OAuth 发现的客户端。

自动认证要求 `auth.enabled: true`，且 `auth.resource_url` 必须是客户端实际访问的 HTTPS 地址。Casdoor 需要提供 OAuth 2.1 Authorization Code + PKCE，并建议支持 Dynamic Client Registration；否则客户端可能要求手工填写 Client ID。

### 1. Claude Desktop

在 **Settings → Connectors → Add custom connector** 中只填写：

```text
https://echoa.example.com/sse
```

保存后点击 **Connect**，Claude Desktop 会打开浏览器完成 Casdoor 登录，并自动保存和刷新 Token。不要在连接器中填写 Bearer Token。

如果使用配置文件（macOS 位于 `~/Library/Application Support/Claude/claude_desktop_config.json`，Windows 位于 `%APPDATA%\Claude\claude_desktop_config.json`），同样只写 URL：

#### OAuth 自动认证（推荐）
```json
{
  "mcpServers": {
    "echoa": {
      "type": "sse",
      "url": "https://echoa.example.com/sse"
    }
  }
}
```

#### 本地调试模式（`auth.enabled: false` 匿名）
```json
{
  "mcpServers": {
    "echoa": {
      "type": "sse",
      "url": "http://127.0.0.1:8080/sse"
    }
  }
}
```

### 2. Cursor

进入 **Settings → Features → MCP Servers → Add new MCP server**：

- **Name**: `echoa`
- **Type**: `SSE`
- **URL**: `https://echoa.example.com/sse`

保存后 Cursor 会根据 401 挑战启动浏览器登录。Headers 留空；预置 `Authorization` 会让 Cursor 跳过自动登录流程。

### 3. Cline / Roo Code（VS Code 插件）

编辑 `cline_mcp_settings.json`，不要添加 `headers`：

```json
{
  "mcpServers": {
    "echoa": {
      "url": "https://echoa.example.com/sse",
      "disabled": false,
      "autoApprove": [
        "mysql_list_tables",
        "mysql_describe_table",
        "redis_scan_keys",
        "redis_list_dbs"
      ]
    }
  }
}
```

保存后在插件的 MCP 列表中点击连接，按提示完成 Casdoor 登录。

### 手工 Bearer Token（不推荐）

仅当客户端不支持 OAuth 发现，或需要在脚本中使用固定凭证时才使用：

```json
{
  "mcpServers": {
    "echoa": {
      "type": "sse",
      "url": "https://echoa.example.com/sse",
      "headers": {
        "Authorization": "Bearer <YOUR_CASDOOR_JWT_TOKEN>"
      }
    }
  }
}
```

Token 过期后需要手工替换。URL Query 中的 `token` 或 `access_token` 仅作为老旧客户端兼容，服务端会记录安全告警，不要用于新配置。

---

## 代码目录结构

```
.
├── cmd/
│   └── server/
│       └── main.go                  # 程序主入口：配置加载、模块注册、Hertz 路由挂载、平滑停机
├── config/
│   └── config.yaml                  # 默认主配置文件（含各安全参数与环境变量占位符）
├── internal/
│   ├── auth/
│   │   ├── casdoor.go               # Casdoor JWT 验签中间件、敏感日志脱敏
│   │   ├── prm.go                   # RFC 9728 保护资源元数据 (PRM) 响应结构与 Handler
│   │   └── prm_test.go              # PRM 逻辑单元测试
│   ├── config/
│   │   └── config.go                # Viper 配置映射、环境变量正则解析与默认兜底注入
│   ├── mcp/
│   │   ├── dispatcher.go            # JSON-RPC 消息分发（initialize, tools/list, tools/call）与审计
│   │   ├── handler_message.go       # POST /message 处理器与并发/队列治理
│   │   ├── handler_sse.go           # GET /sse 处理器、会话分配与 25s 心跳保活
│   │   ├── integration_test.go      # MCP SSE/Message 端到端全流程集成测试
│   │   ├── server.go                # MCP 服务端核心上下文与反代路由前缀处理
│   │   ├── session.go               # Session 生命周期、背压熔断控制
│   │   ├── tool.go                  # 工具注册中心 (ToolRegistry)
│   │   └── types.go                 # MCP 规范 JSON-RPC 协议与能力结构体定义
│   └── modules/
│       ├── mysql/
│       │   ├── module.go            # MySQL 模块初始化、连接池生命周期与工具注册
│       │   ├── parser.go            # 基于 TiDB Parser 的只读 AST 审查、表黑名单与 LIMIT 改写
│       │   ├── parser_test.go       # SQL 语法安全审查及 LIMIT 改写单元测试
│       │   └── tools.go             # mysql_list_tables / describe / query 实现与 64KB 截断
│       └── redis/
│           ├── manager.go           # MultiDBManager 多 DB 独立连接池懒加载管理器
│           ├── module.go            # Redis 模块初始化、命令白名单映射与工具注册
│           ├── tools.go             # redis_execute / scan_keys / list_dbs 实现与集合截断
│           └── tools_test.go        # Redis 命令拦截、白名单过滤及截断单元测试
├── scripts/
│   └── init.sql                     # Docker Compose 本地联调数据库初始化与黑名单测试数据
├── .github/workflows/
│   └── image.yml                    # GitHub Actions 多架构容器镜像构建与发布工作流
├── Dockerfile                       # 多阶段轻量 Alpine 生产镜像构建
├── docker-compose.yml               # 本地一键联调集成编排文件
├── Makefile                         # 常用编译、测试、镜像构建快捷指令
└── README.md                        # 项目官方文档与使用说明
```

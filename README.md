# Echoa

基于 **Golang 原生 `net/http` + `http.NewServeMux`** 构建的企业级只读数据库 Model Context Protocol (MCP) 服务端。内置 **Casdoor OAuth 2.1 鉴权** 与 **RFC 9728 保护资源元数据发现**，为 AI Agent（如 Claude Desktop、Cursor、Cline、LibreChat 等）提供安全受控、深度隔离的 **MySQL** 与 **Redis** 只读分析能力。

参考规范与标准：
- [MCP Specification (Streamable HTTP Transport)](https://modelcontextprotocol.io/)
- [RFC 9728: OAuth 2.0 Protected Resource Metadata](https://datatracker.ietf.org/doc/html/rfc9728)
- [googleapis/mcp-toolbox](https://github.com/googleapis/mcp-toolbox)
- [t8y2/dbx](https://github.com/t8y2/dbx)

---

## 目录

- [核心架构特性](#核心架构特性)
- [MCP 工具清单与参数定义](#mcp-工具清单与参数定义)
  - [MySQL 工具](#1-mysql-工具)
  - [Redis 工具](#2-redis-工具)
- [HTTP 端点速查](#http-端点速查)
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

1. **双代兼容（Dual-Era）与原生无状态 Streamable HTTP 传输**：基于 Go 标准库 `net/http` 原生 `http.NewServeMux` 与标准 JSON-RPC 2.0，零第三方 Web 框架开销。既支持主流客户端（Cursor、Claude Desktop、Cline）标准的 `initialize` 握手、`notifications/initialized`、`ping` 生命周期与常规工具调用，也完整支持 MCP 2026-07-28 无状态 Streamable HTTP 规范与 `server/discover` 端点。无会话状态依赖，兼容各类反向代理与水平扩展。
2. **标准 Casdoor OAuth 2.1 鉴权与 JWKS 自动轮转**：
   - 支持动态获取 Casdoor OIDC JWKS（`/.well-known/jwks`）公钥集合并实现内存缓存与自动公钥轮转，网络故障时平滑回退至静态 Certificate PEM。
   - 强制校验 JWT `Audience`（`aud`），杜绝 Casdoor 同一实例下不同 Client ID 跨应用伪造与越权风险。
   - 严密拦截 OAuth Refresh Token 冒充 Access Token。
   - 严格遵循 RFC 6750，通过 `Authorization: Bearer <jwt>` 标头进行无状态验签。
   - 兼容老旧客户端通过 URL Query（`?token=...` / `?access_token=...`）传参的模式（已标记为 Deprecated 并记录安全告警）。
   - 实现 RFC 9728 规范的 Protected Resource Metadata (`/.well-known/oauth-protected-resource`)，支持客户端自动发现认证服务器与 Scope。
3. **MySQL 深度 AST 防御体系**：
   - 基于 `pingcap/tidb/pkg/parser` 对传入 SQL 做抽象语法树（AST）强校验，仅允许 `SELECT`、`EXPLAIN`、`SHOW`。
   - 彻底拦截 `SELECT ... FOR UPDATE`、`LOCK IN SHARE MODE` 等加锁读取，以及 `SELECT ... INTO OUTFILE / DUMPFILE` 等服务端写盘提权语句。
   - 智能 LIMIT 注入与改写：无 LIMIT 自动追加系统上限；超过配置上限强制压低改写；保留原分页 offset；子查询与外层查询严格隔离改写。
   - 敏感数据表黑名单过滤（支持 `table` 与 `schema.table` 精准匹配拦截）。
   - 双重防撑爆防护：单单元格 4KB 缩略截断 + 结果集 64KB 字符级硬上限。
4. **Redis 6 ACL 鉴权与多 DB 独立隔离治理**：
   - 支持 Redis 6+ ACL 用户名（`username`）与密码认证，原生适配腾讯云等只读 ACL 账号（如 `mcp_ro`）。
   - 动态客户端管理器（`MultiDBManager`），每个逻辑 DB 独立连接池懒加载并继承 ACL 凭证，**杜绝并发执行 `SELECT` 导致的数据串库重大隐患**。
   - 完备连接池生命周期参数（`pool_size`、`min_idle_conns`、10m 空闲超时、1h 存活淘汰），防御云原生网络下的静默断连。
   - 统一命令网关 `redis_execute`，内置命令白名单与硬黑名单防御（阻断 `KEYS`、`FLUSHALL`、`CONFIG`、写操作及手动切库 `SELECT`）。
   - 多类型（`[]string`、`map[string]string`、RESP3 `map[any]any`）双重截断保护（条目上限 + 64KB 响应硬上限）。
5. **按 endpoint 隔离的数据源**：
   - `sources` 按名字声明任意数量的 MySQL 与 Redis 连接，`endpoints` 决定每个连接暴露在哪个路径。
   - 每个 endpoint 是一套独立的无状态 MCP 服务和工具表：`POST /mysql1` 只看得到它引用的 source 的工具，互不干扰。
   - 一个 endpoint 只挂一个 MySQL 时工具名保持 `mysql_query`；同一个 endpoint 挂多个同种类 source 时，工具名带上 source 名字（`orders_mysql_query`），避免互相覆盖。
6. **无状态传输韧性与可观测性**：
   - 遵循 MCP 2026-07-28 无状态 Streamable HTTP 规范：无握手、无 Session 状态依赖，通过 `server/discover` 进行能力与版本发现，每个请求通过 HTTP 标头及 `_meta` 携带协议版本与方法校验。
   - 单主体并发限制（默认最多 5 并发）与 HTTP 429 背压，按 endpoint + 鉴权主体（JWT subject / developer）隔离治理。
   - DNS Rebinding 保护（Origin 头严格校验）。
   - 结构化 JSON 审计日志（自动对 `password`、`token`、`secret` 等敏感参数脱敏，单参数超 200 字符缩略）。
   - K8s 存活（`/health`）与就绪（`/health/ready`）深度数据源探活探针。

---

## MCP 工具清单与参数定义

每个 endpoint 向 MCP 客户端暴露它引用的 source 对应的工具。一个 endpoint 只挂一个 MySQL 时是下面这 3 个；Redis 同理。

### 1. MySQL 工具

#### `mysql_list_databases`
列出当前 MySQL 实例中所有可访问的业务数据库（schema），自动过滤内部系统库（`information_schema`、`mysql`、`performance_schema`、`sys`）。
- **参数**：
  - `pattern` (`string`, 可选): 可选的 SQL LIKE 过滤模式（如 `"order_%"`、`"%test%"`）。
- **返回示例**：
  ```json
  {
    "total_databases": 2,
    "databases": [
      { "name": "billing", "charset": "utf8mb4", "collation": "utf8mb4_unicode_ci" },
      { "name": "orders", "charset": "utf8mb4", "collation": "utf8mb4_unicode_ci" }
    ],
    "current_database": "orders"
  }
  ```

#### `mysql_list_tables`
列出指定或当前数据库的所有可用表、类型、存储引擎、预估行数及表注释（自动过滤配置黑名单中的敏感表）。
- **参数**：
  - `schema` (`string`, 可选): 待查的目标数据库/schema 名称。
    - **指定时**：查询该数据库下的所有数据表。
    - **未指定时**：若连接已配置默认数据库，查询默认库中的表；若连接未配置默认库（如 DSN 仅连到根路径 `/`），自动列出全部业务库中的表并在结果中标明所属 `schema`。
- **返回示例**：
  ```json
  {
    "total_tables": 2,
    "tables": [
      {
        "schema": "orders",
        "name": "orders",
        "type": "BASE TABLE",
        "engine": "InnoDB",
        "rows_estimate": 4,
        "comment": "业务订单表"
      },
      {
        "schema": "orders",
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
  - `table_name` (`string`, 必填): 目标表名。支持普通表名 `users` 或跨库表名 `schema.table`（如 `billing.orders`）。
  - `schema` (`string`, 可选): 目标数据库/schema 名称。当 `table_name` 已经包含 `.` 时本参数被忽略。若两者均未提供，回退到连接默认库。
- **返回示例**：
  ```json
  {
    "schema": "billing",
    "table": "orders",
    "columns": [
      { "field": "id", "type": "bigint", "null": "NO", "key": "PRI", "default": "", "extra": "auto_increment", "comment": "" },
      { "field": "order_no", "type": "varchar(64)", "null": "NO", "key": "UNI", "default": "", "extra": "", "comment": "订单号" },
      { "field": "amount", "type": "decimal(10,2)", "null": "NO", "key": "", "default": "0.00", "extra": "", "comment": "订单金额" }
    ]
  }
  ```

#### `mysql_query`
执行只读 SQL 查询。服务端进行 TiDB AST 审查，仅允许 `SELECT`、`EXPLAIN`、`SHOW`，并自动追加或改写 `LIMIT`。
- **参数**：
  - `query` (`string`, 必填): 待执行的 SQL 语句。支持同一实例下的跨库查询（例如 `SELECT * FROM billing.orders JOIN crm.users ON ...`）。
  - `schema` (`string`, 可选): 可选的默认数据库/schema 名称。若传入，服务端会在 AST 解析阶段自动为未带前缀的数据表补充限定（例如把 `SELECT * FROM orders` 自动补全为 `SELECT * FROM billing.orders`），**杜绝在连接池复用环境下调用 `USE db` 引发的连接污染隐患**。
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

## HTTP 端点速查

| 路径 | 方法 | 鉴权要求 | 说明 |
| :--- | :--- | :--- | :--- |
| `<endpoint>` | `POST` | 依配置（Bearer JWT） | MCP 双代兼容 Streamable HTTP JSON-RPC 消息入口（兼容主流客户端 `initialize`/`ping` 与 2026-07-28 规范）。`<endpoint>` 来自配置的 `endpoints[].path`（如 `/mysql`、`/redis`）。GET/DELETE 会明确返回 405 |
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
             [ 2. MCP 无状态并发治理 ]
              Origin DNS Rebinding 校验 (HTTP 403)
              并发限流 (5 并发/主体) + HTTP 429
              结构化脱敏审计日志 (JSON)
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
  allowed_origins: []           # 允许的 Origin 白名单 (留空仅放行无 Origin 与 localhost / 127.0.0.1)
  max_inflight_per_principal: 5 # 每个 endpoint 单个主体最大并发请求数 (超出 429)

# Casdoor OAuth 2.1 鉴权配置
auth:
  enabled: false                # 本地开发测试置为 false (匿名调试)，生产环境必须设为 true
  resource_url: "${MCP_RESOURCE_URL:-http://127.0.0.1:8080}" # 服务对外公开 URL
  endpoint: "https://casdoor.example.com"                   # Casdoor 实例根地址
  client_id: "mcp-service"                                  # Casdoor 客户端 ID
  client_secret: "${CASDOOR_CLIENT_SECRET}"                 # 客户端密钥
  certificate: "${CASDOOR_JWT_CERT}"                       # Casdoor 应用的 x509 JWT 公钥证书内容 (兜底)
  audience: "${CASDOOR_AUDIENCE:-}"                        # 预期 JWT Audience (aud)，默认取 client_id
  jwks_uri: "${CASDOOR_JWKS_URI:-}"                        # Casdoor JWKS 端点 (留空推导为 <endpoint>/.well-known/jwks)
  organization_name: "your-org"                             # 组织名称
  application_name: "mcp-app"                               # 应用名称
  scopes_required:                                          # 要求的权限范围
    - "openid"
    - "offline_access"

# 可命名的只读数据源。名字在 mysql 与 redis 之间全局唯一，endpoint 按名字引用。
sources:
  mysql:
    mysql-main:
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
    redis-main:
      addr: "${REDIS_ADDR:-127.0.0.1:6379}"
      username: "${REDIS_USERNAME:-}"                       # Redis 6 ACL 用户名 (如腾讯云只读账号 mcp_ro)
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

# 每个 endpoint 是一个独立的 MCP 服务，只暴露它引用的 source 的工具。
endpoints:
  - path: /mysql                 # 客户端连接 POST http://host:8080/mysql
    sources: [mysql-main]
  - path: /redis
    sources: [redis-main]
```

### 多个同类数据源

同一个种类可以声明多份，每份给一个自己的 endpoint。两个 MySQL 就是两段 source 加两个 path：

```yaml
sources:
  mysql:
    orders:
      dsn: "${MYSQL_ORDERS_DSN}"
      security:
        max_limit: 200
    billing:
      dsn: "${MYSQL_BILLING_DSN}"
      security:
        max_limit: 100
        table_blacklist: ["payment_secret"]

endpoints:
  - path: /mysql1               # 客户端发送 POST http://host:8080/mysql1 ，工具名为 mysql_query
    sources: [orders]
  - path: /mysql2               # 客户端发送 POST http://host:8080/mysql2 ，同样是 mysql_query，连的是另一个库
    sources: [billing]
```

隔离边界是 endpoint：`POST /mysql1` 的 `tools/list` 看不到 billing 的任何工具，两边的并发限流配额也互不影响。

同一个 endpoint 挂两个同种类 source 也可以，此时工具名会带上 source 名字以免互相覆盖，例如 `orders_mysql_query` 与 `billing_mysql_query`：

```yaml
endpoints:
  - path: /mysql
    sources: [orders, billing]
```

约束：source 名字在全部种类之间唯一（不能同时有一个叫 `main` 的 MySQL 和一个叫 `main` 的 Redis）；`path` 只能是单段（`/mysql1` 可以，`/db/mysql` 不行），且不能占用 `/health`。同一个 source 被多个 endpoint 引用时只建一个连接池。

### 关键环境变量对照表

| 环境变量 | 对应配置项 | 默认值 / 示例 |
| :--- | :--- | :--- |
| `MCP_RESOURCE_URL` | `auth.resource_url` | `http://127.0.0.1:8080` |
| `CASDOOR_CLIENT_SECRET` | `auth.client_secret` | - |
| `CASDOOR_JWT_CERT` | `auth.certificate` | Casdoor x509 证书内容 (静态兜底) |
| `CASDOOR_AUDIENCE` | `auth.audience` | 预期 JWT Audience (如 `f983ca0f75f64773a09c`) |
| `CASDOOR_JWKS_URI` | `auth.jwks_uri` | `https://casdoor.example.com/.well-known/jwks` |
| `MYSQL_DSN` | `sources.mysql.mysql-main.dsn` | `root:password@tcp(127.0.0.1:3306)/test?...` |
| `REDIS_ADDR` | `sources.redis.redis-main.addr` | `127.0.0.1:6379` |
| `REDIS_USERNAME` | `sources.redis.redis-main.username` | Redis 6 ACL 用户名 (如 `mcp_ro`) |
| `REDIS_PASSWORD` | `sources.redis.redis-main.password` | 空 |

这三项只是默认配置用的占位符。自己加的 source 用什么环境变量，由那段配置里写的 `${...}` 决定，例如 `${MYSQL_ORDERS_DSN}`。

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

> **协议版本与客户端兼容性提示**：
> Echoa 采用**双代客户端兼容（Dual-Era）架构**，同时支持主流标准客户端与现代无状态客户端：
> - **主流标准客户端兼容（Cursor, Claude Desktop, Cline 等）**：
>   - 原生支持标准 JSON-RPC 2.0 生命周期的 `initialize` 握手（自动协商受支持版本 `2024-11-05` ~ `2025-11-25`）、`notifications/initialized` 与 `ping` 保活。
>   - 标准工具列表（`tools/list`）与调用（`tools/call`）无需携带 `Mcp-Method` / `Mcp-Name` 自定义请求头及 `_meta` 字段，主流客户端配置 URL 即可即插即用。
> - **现代无状态客户端（MCP 2026-07-28 规范）**：
>   - 显式声明 `MCP-Protocol-Version: 2026-07-28` 或 `Mcp-Method` 的现代客户端，启用每请求严格标头一致性与 `_meta` 校验。
>   - 支持通过 `server/discover` 端点免握手自省服务能力、指令与缓存新鲜度（TTL）。

推荐使用 **OAuth 自动认证**：配置中只写服务 URL，不要预置 `Authorization`。客户端访问时收到 HTTP 401，从 `WWW-Authenticate` 中的 `resource_metadata` 发现 Casdoor，再打开浏览器完成登录。手工 Bearer Token 仅用于调试、自动化脚本，或不支持 OAuth 发现的客户端。

自动认证要求 `auth.enabled: true`，且 `auth.resource_url` 必须是客户端实际访问的 HTTPS 地址。Casdoor 需要提供 OAuth 2.1 Authorization Code + PKCE，并建议支持 Dynamic Client Registration；否则客户端可能要求手工填写 Client ID。

URL 指向某个 endpoint 的挂载路径（如 `/mysql`、`/redis`）。默认配置下 MySQL 是 `/mysql`，Redis 是 `/redis`；加了第二个 MySQL 后，再加一条客户端配置指向 `/mysql2`。下面的示例都用 MySQL。

### 1. Claude Desktop

在 **Settings → Connectors → Add custom connector** 中只填写：

```text
https://echoa.example.com/mysql
```

保存后点击 **Connect**，Claude Desktop 会打开浏览器完成 Casdoor 登录，并自动保存和刷新 Token。不要在连接器中填写 Bearer Token。

如果使用配置文件（macOS 位于 `~/Library/Application Support/Claude/claude_desktop_config.json`，Windows 位于 `%APPDATA%\Claude\claude_desktop_config.json`），同样只写 URL：

#### OAuth 自动认证（推荐）
```json
{
  "mcpServers": {
    "echoa": {
      "type": "http",
      "url": "https://echoa.example.com/mysql"
    }
  }
}
```

#### 本地调试模式（`auth.enabled: false` 匿名）
```json
{
  "mcpServers": {
    "echoa": {
      "type": "http",
      "url": "http://127.0.0.1:8080/mysql"
    }
  }
}
```

### 2. Cursor

进入 **Settings → Features → MCP Servers → Add new MCP server**：

- **Name**: `echoa`
- **Type**: `HTTP` (Streamable HTTP)
- **URL**: `https://echoa.example.com/mysql`

保存后 Cursor 会根据 401 挑战启动浏览器登录。Headers 留空；预置 `Authorization` 会让 Cursor 跳过自动登录流程。

### 3. Cline / Roo Code（VS Code 插件）

编辑 `cline_mcp_settings.json`，不要添加 `headers`：

```json
{
  "mcpServers": {
    "echoa": {
      "url": "https://echoa.example.com/mysql",
      "disabled": false,
      "autoApprove": [
        "mysql_list_databases",
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
      "type": "http",
      "url": "https://echoa.example.com/mysql",
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
│       └── main.go                  # 程序主入口：配置加载、按 endpoint 注册模块、ServeMux 路由挂载、平滑停机
├── config/
│   └── config.yaml                  # 默认主配置文件（含各安全参数与环境变量占位符）
├── docs/
│   └── mcp-2026-07-28-migration.md  # MCP 2026-07-28 无状态迁移方案说明
├── internal/
│   ├── auth/
│   │   ├── casdoor.go               # Casdoor JWT 验签中间件、敏感日志脱敏
│   │   ├── prm.go                   # RFC 9728 保护资源元数据 (PRM) 响应结构与 Handler
│   │   └── prm_test.go              # PRM 逻辑单元测试
│   ├── config/
│   │   ├── config.go                # Viper 配置映射、环境变量正则解析与校验
│   │   └── config_test.go           # 配置加载与端点路由校验单元测试
│   ├── mcp/
│   │   ├── integration_test.go      # MCP 2026-07-28 无状态 HTTP 全流程集成测试
│   │   ├── limiter.go               # 基于鉴权主体的并发限流器 (InflightLimiter)
│   │   ├── server.go                # MCP 无状态服务端核心 Handler、头校验与 RPC 分发
│   │   ├── tool.go                  # 工具注册中心 (ToolRegistry)
│   │   └── types.go                 # MCP 2026-07-28 规范 JSON-RPC 协议与能力结构体定义
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
```
├── docker-compose.yml               # 本地一键联调集成编排文件
├── Makefile                         # 常用编译、测试、镜像构建快捷指令
└── README.md                        # 项目官方文档与使用说明
```

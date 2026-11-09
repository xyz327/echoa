# Echoa

基于 **Golang 原生 `net/http` + `http.NewServeMux`** 构建的企业级只读数据库 Model Context Protocol (MCP) 服务端。内置 **Casdoor OAuth 2.1 鉴权** 与 **RFC 9728 保护资源元数据发现**，为 AI Agent（如 Claude Desktop、Cursor、Cline 等）提供安全受控、深度隔离的 **MySQL** 与 **Redis** 只读分析能力。

参考规范与标准：
- [MCP Specification (Streamable HTTP Transport)](https://modelcontextprotocol.io/)
- [RFC 9728: OAuth 2.0 Protected Resource Metadata](https://datatracker.ietf.org/doc/html/rfc9728)

---

## 核心架构特性

- **双代兼容（Dual-Era）与无状态 HTTP 传输**：零第三方 Web 框架依赖。既支持主流客户端（Cursor、Claude Desktop、Cline）标准的 `initialize`/`ping` 生命周期，也支持 MCP 2026-07-28 无状态 Streamable HTTP 规范与 `server/discover` 端点。
- **插件化数据源驱动工厂（DataSource Factory）**：通过统一 `datasource.Instance` 接口与自动注册工厂解耦具体数据源实现与核心路由，支持任意数量 MySQL/Redis 实例按 endpoint 独立隔离挂载。
- **MySQL 深度 AST 防御体系**：
  - 基于 TiDB Parser 对 SQL 做抽象语法树（AST）强校验，仅允许 `SELECT`、`EXPLAIN`、`SHOW`；使用 `sync.Pool` 复用解析器以降低 GC 开销。
  - 阻断加锁读取（`FOR UPDATE`、`LOCK IN SHARE MODE`）与写盘提权（`INTO OUTFILE`）。
  - 严密拦截高危/DoS 函数：`SLEEP`（拒绝服务与时间盲注）、`BENCHMARK`（CPU 耗尽）、`LOAD_FILE`（主机磁盘探测）、`GET_LOCK`/`RELEASE_LOCK`（锁竞争与会话阻塞）、`SYS_EVAL` 等 UDF 执行函数。
  - 智能 LIMIT 注入与改写：无 LIMIT 自动追加系统上限；超过配置强制压低改写；`EXPLAIN ANALYZE` 同等注入 LIMIT；OFFSET 超限直接阻断深分页全表扫描。
  - 跨库查询支持：AST 自动按配置补充 Schema 限定，**杜绝并发调用 `USE db` 导致连接池污染**。
  - 结果集防撑爆：单单元格 4KB 截断 + 增量 $O(1)$ 累计字节计算，严格守住 64KB 字符级响应硬上限。
- **Redis 6 ACL 与多 DB 隔离治理**：
  - 支持 ACL 只读账号；`MultiDBManager` 为每个逻辑 DB 提供独立连接池，**杜绝并发 `SELECT` 导致数据串库**。
  - 查询侧参数前置收紧：`LRANGE`/`ZRANGE` 索引窗口收敛、`SCAN`/`HSCAN` COUNT 压制、`MGET` 键数量截断，在发送至 Redis 前收缩范围。
  - 命令白名单与硬黑名单防御（阻断 `KEYS`、`FLUSHALL`、`CONFIG`、写操作及切库 `SELECT`）。
- **Casdoor OAuth 2.1 鉴权与 Singleflight 缓存**：
  - 支持 JWKS 动态拉取与内存缓存，引入 `singleflight` 防止冷启动或轮转时的缓存击穿风暴。
  - 强制校验 Audience（`aud`），拦截 Refresh Token 伪造；实现 RFC 9728 PRM 发现接口。
- **全链路结构化可观测性**：基于 Go 原生 `log/slog`，自动提取/生成 `X-Request-ID` 透传追踪链路，提供脱敏 JSON 审计日志与 K8s 存活（`/health`）/ 就绪（`/health/ready`）探针。

---

## MCP 工具清单

### 1. MySQL 工具

| 工具名 | 参数说明 | 功能与防御机制 |
| :--- | :--- | :--- |
| `mysql_list_databases` | `pattern` (string, 可选): LIKE 过滤模式 | 列出实例上所有业务数据库，自动屏蔽 `information_schema` 等系统库。 |
| `mysql_list_tables` | `schema` (string, 可选): 目标数据库 | 列出可用表、类型、引擎、预估行数与注释，自动过滤黑名单表。 |
| `mysql_describe_table` | `table_name` (string, 必填), `schema` (string, 可选) | 查看指定表字段结构（字段名、类型、NULL、索引键、默认值、注释）。 |
| `mysql_query` | `query` (string, 必填), `schema` (string, 可选), `max_rows` (int, 可选) | 深度 AST 只读审查；强制改写/注入 `LIMIT`；拦截高危函数与黑名单表；单单元格 4KB 截断与 64KB 硬上限。 |

### 2. Redis 工具

| 工具名 | 参数说明 | 功能与防御机制 |
| :--- | :--- | :--- |
| `redis_execute` | `command` (string, 必填), `args` (array, 必填), `db` (int, 默认 0) | 统一只读命令网关（`GET`, `HGET`, `LRANGE`, `ZRANGE`, `TTL`, `TYPE` 等）。执行前按语义收紧下标/COUNT 参数；阻断 `KEYS` 与写操作。 |
| `redis_scan_keys` | `pattern` (string, 必填), `limit` (int, 默认 50), `db` (int, 默认 0) | 基于游标 `SCAN` 安全遍历 Key，分批迭代，杜绝生产环境阻塞。 |
| `redis_list_dbs` | 无 | 读取 `INFO keyspace`，查看各逻辑库 Key 数量分布与过期统计。 |

---

## HTTP 端点速查

| 路径 | 方法 | 鉴权 | 说明 |
| :--- | :--- | :--- | :--- |
| `<endpoint>` | `POST` | Bearer JWT / 可配置免密 | MCP Streamable HTTP JSON-RPC 消息入口（如 `/mysql`、`/redis`）。GET/DELETE 返回 405。 |
| `/.well-known/oauth-protected-resource` | `GET` | 公开 | **RFC 9728 PRM** 元数据，供 AI 客户端自动发现 OAuth 认证端点与 Scope。 |
| `/health` | `GET` | 公开 | K8s 存活探针 (Liveness)，返回 HTTP 200 `{"status":"ok"}`。 |
| `/health/ready` | `GET` | 公开 | K8s 就绪探针 (Readiness)，实际探测 MySQL/Redis 联通性，异常时返回 503。 |

---

## 配置说明

配置文件位于 `config/config.yaml`，原生支持 `${ENV:-default}` 环境变量注入：

```yaml
server:
  port: 8080
  read_timeout_ms: 15000
  allowed_origins: []           # 留空放行无 Origin 与 localhost
  max_inflight_per_principal: 5 # 每个 endpoint 单主体并发上限 (超限返回 429)

auth:
  enabled: false                # 本地调试置为 false；生产环境建议开启
  resource_url: "${MCP_RESOURCE_URL:-http://127.0.0.1:8080}"
  endpoint: "https://casdoor.example.com"
  client_id: "mcp-service"
  client_secret: "${CASDOOR_CLIENT_SECRET}"
  certificate: "${CASDOOR_JWT_CERT}"
  audience: "${CASDOOR_AUDIENCE:-}"

sources:
  mysql:
    mysql-main:
      dsn: "${MYSQL_DSN:-root:password@tcp(127.0.0.1:3306)/test?charset=utf8mb4&parseTime=True&loc=Local&multiStatements=false}"
      max_open_conns: 20
      max_idle_conns: 5
      query_timeout_ms: 5000
      security:
        max_limit: 200          # 单次查询强制限制的最大返回行数
        max_offset: 100000      # 分页 OFFSET 上限，超出直接拒绝（防深分页全表扫描）
        table_blacklist: ["user_password", "sys_secret"]

  redis:
    redis-main:
      addr: "${REDIS_ADDR:-127.0.0.1:6379}"
      username: "${REDIS_USERNAME:-}" # 支持 Redis 6 ACL 用户名
      password: "${REDIS_PASSWORD:-}"
      allowed_dbs: [0, 1, 2, 3]
      pool_size: 20
      max_items_limit: 200
      query_timeout_ms: 3000
      security:
        allowed_commands: ["GET", "MGET", "HGET", "HSCAN", "LRANGE", "ZRANGE", "SCAN", "TYPE", "TTL", "PING"]
        blocked_commands: ["KEYS", "MONITOR", "INFO"]

# 每个 endpoint 是一套独立的 MCP 服务；同类多个 source 挂载至同一 endpoint 时工具名前缀会自动限定
endpoints:
  - path: /mysql
    sources: [mysql-main]
  - path: /redis
    sources: [redis-main]
```

---

## 快速开始

### 1. 本地原生运行

要求环境：Go 1.22+。

```bash
# 运行单元测试
make test

# 编译二进制可执行文件
make build

# 启动服务 (支持 -log-level=info|debug -log-format=json|text)
./bin/server -config config/config.yaml -log-format text
```

### 2. Docker Compose 一键全套联调

项目根目录包含完整的编排文件与预置数据库脚本（`scripts/init.sql`），一键拉起 MySQL 8.0、Redis 7.0 与 Echoa：

```bash
# 启动全套服务
make compose-up

# 查看服务状态与就绪探活
docker compose ps
curl -i http://127.0.0.1:8080/health/ready

# 停止全套服务
make compose-down
```

---

## 客户端接入配置

推荐使用 **OAuth 自动认证**：配置中只填写服务 URL。客户端收到 HTTP 401 后会自动从 `resource_metadata` 发现 Casdoor 并拉起浏览器完成登录。

| 客户端 | 接入路径 | 配置方式 |
| :--- | :--- | :--- |
| **Claude Desktop** | Settings → Connectors → Add custom connector | 填入 `https://echoa.example.com/mysql` 即可，保存后点击 Connect 完成登录。 |
| **Cursor** | Settings → Features → MCP Servers → Add new | Name: `echoa`, Type: `HTTP`, URL: `https://echoa.example.com/mysql`（Headers 留空）。 |
| **Cline / Roo Code** | `cline_mcp_settings.json` | 见下方 JSON 配置示例。 |

```json
{
  "mcpServers": {
    "echoa-mysql": {
      "type": "http",
      "url": "https://echoa.example.com/mysql"
    },
    "echoa-redis": {
      "type": "http",
      "url": "https://echoa.example.com/redis"
    }
  }
}
```

> **本地无鉴权模式**：将 `config.yaml` 中 `auth.enabled` 设为 `false`，URL 填 `http://127.0.0.1:8080/mysql` 即可匿名调用。

---

## 代码目录结构

```
.
├── cmd/server/main.go            # 服务主入口：配置加载、工厂实例化、路由分发与平滑停机
├── config/config.yaml            # 默认主配置文件
├── internal/
│   ├── auth/                     # Casdoor OAuth 2.1 验签、RFC 9728 PRM、JWKS Singleflight
│   ├── config/                   # Viper 配置加载与环境变量解析
│   ├── datasource/               # 统一数据源驱动工厂 (DataSource Factory 抽象)
│   ├── logger/                   # log/slog 结构化日志封装与 Request-ID 上下文透传
│   ├── mcp/                      # MCP 2026-07-28 & 兼容层 Server、RPC 分发、并发限流器
│   └── modules/
│       ├── mysql/                # MySQL 驱动实现、TiDB AST 深度审查与 LIMIT 改写
│       └── redis/                # Redis 驱动实现、MultiDBManager 连接池、前置参数钳制
├── Dockerfile                    # 多阶段构建轻量 Alpine 生产镜像
├── docker-compose.yml            # 本地联调编排文件
└── Makefile                      # 编译、测试、镜像构建快捷指令
```

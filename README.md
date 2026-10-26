# Echoa

基于 **Golang + CloudWeGo Hertz + `hertz-contrib/sse`** 构建的企业级 Model Context Protocol (MCP) 服务端，支持 **Casdoor OAuth 鉴权**，为 AI Agent 提供安全可控的 **MySQL** 与 **Redis** 只读能力。

参考标准：
- [MCP Specification (SSE Transport)](https://modelcontextprotocol.io/)
- [googleapis/mcp-toolbox](https://github.com/googleapis/mcp-toolbox)
- [t8y2/dbx](https://github.com/t8y2/dbx)

---

## 核心架构特性

1. **高性能流式通信**：基于 CloudWeGo Hertz 原生网络栈与 `hertz-contrib/sse`，直接利用连接劫持（HijackWriter）与分块流式输出（Chunked Writer），极低内存占用与高并发连接支撑。
2. **Casdoor OAuth 鉴权**：支持标准的 `Authorization: Bearer <jwt>` 标头（推荐方式，严格遵循 RFC 6750 与 RFC 9728）。URL Query（`?token=...`）作为老旧浏览器 EventSource 的过渡兼容方案已标记为 Deprecated，服务端会输出告警。
3. **MySQL 双重只读防御体系**：
   - 基于 `pingcap/tidb/pkg/parser` 抽象语法树（AST）强校验，仅允许 `SELECT`、`EXPLAIN`、`SHOW` 语句。
   - 彻底拦截 `SELECT ... FOR UPDATE`、`LOCK IN SHARE MODE` 等加锁读取与 `SELECT ... INTO OUTFILE / DUMPFILE` 服务端写盘语句。
   - 彻底基于 AST 节点注入与兜底 `LIMIT`（无 LIMIT 自动追加，超过配置上限强制改写，保留分页 offset，子查询与外层查询严格隔离）。
   - 敏感数据表黑名单过滤（支持单表名与 `schema.table` 跨库精准屏蔽）。
   - 结果集 64KB 字符级硬上限与单元格 4KB 缩略截断机制，双重避免大文本撑满 LLM 上下文。
4. **Redis 多 DB 隔离与连接池治理**：
   - 动态客户端管理器（`MultiDBManager`），每个 DB 独立连接池懒加载，杜绝并发请求执行 `SELECT` 导致的数据串库 Bug。
   - 完善连接池生命周期参数（`pool_size`、`min_idle_conns`、10m 空闲超时、1h 存活淘汰），杜绝云原生网络下的静默断连。
   - 统一命令网关 `redis_execute`，结合白名单与硬黑名单防御（阻断 `KEYS`、`FLUSHALL`、`CONFIG` 及写操作）。
   - 全类型（`[]string`、`map[string]string`、`map[any]any` 等）双重截断保护（条目上限 + 64KB 字符级硬上限），防止大集合或大 Hash 挤爆 LLM 上下文。
5. **传输韧性与可观测性**：
   - 会话级并发限制（每 Session 最多 5 并发）与 HTTP 429 背压，后台空闲 15 分钟会话自动回收。
   - SSE 发送通道 5 秒超时熔断防线，杜绝慢客户端或死连接拖垮后端工作协程。
   - 结构化 JSON 审计日志（自动对 Token/密码等敏感参数脱敏，单参数超 200 字符截断）。
   - K8s 标准存活（`/health`）与就绪（`/health/ready`）数据源深度探活探针。
   - SSE 端点反代路由自动前缀（`X-Forwarded-Prefix` / `resource_url`）兼容解析。

---

## 提供的 MCP 工具清单

| 工具名称 | 分类 | 说明 |
| :--- | :--- | :--- |
| `mysql_list_tables` | MySQL | 列出当前数据库的所有表、表类型、预估行数及注释（自动剔除黑名单表） |
| `mysql_describe_table` | MySQL | 查看指定表的字段名、数据类型、是否可为空、键、默认值及注释 |
| `mysql_query` | MySQL | 执行只读 SQL（仅限 SELECT/EXPLAIN/SHOW），内置 AST 审查与 LIMIT 兜底 |
| `redis_execute` | Redis | 统一只读命令执行网关（支持 `GET`, `HGETALL`, `LRANGE`, `SCAN`, `TTL` 等），支持 `db` 参数 |
| `redis_scan_keys` | Redis | 使用 `SCAN` 安全遍历匹配 glob 模式的 Key，防止阻塞 Redis |
| `redis_list_dbs` | Redis | 通过 `INFO keyspace` 查看各逻辑数据库（DB 0~15）的 Key 分布情况 |

---

## 快速开始

### 1. 配置文件
编辑 `config/config.yaml`（支持 `${ENV_NAME}` 环境变量注入）：

```yaml
server:
  port: 8080
auth:
  enabled: false # 本地开发设为 false 开启匿名调试，生产设为 true
  endpoint: "https://casdoor.example.com"
  client_id: "mcp-service"
  client_secret: "${CASDOOR_CLIENT_SECRET}"
  certificate: "${CASDOOR_JWT_CERT}"
  organization_name: "your-org"
  application_name: "mcp-app"
datasources:
  mysql:
    enabled: true
    dsn: "root:password@tcp(127.0.0.1:3306)/test?charset=utf8mb4&parseTime=True&loc=Local&multiStatements=false"
  redis:
    enabled: true
    addr: "127.0.0.1:6379"
    password: ""
    allowed_dbs: [0, 1, 2, 3]
```

### 2. 编译与运行

#### 本地原生运行
```bash
# 编译
make build

# 运行
./bin/server -config config/config.yaml
```

#### Docker Compose 一键全套联调（推荐快速试用）
包含 MySQL 8.0（预置 `users`、`orders` 正常业务表及 `user_password`、`auth_tokens` 黑名单表）、Redis 7.0 及当前 MCP 服务端：

```bash
# 一键拉起全套开发环境并就绪探活
make compose-up
# 或直接运行: docker compose up -d

# 查看各容器状态与健康探活
docker compose ps

# 查看 MCP 实时日志
docker compose logs -f echoa

# 停止并清理容器
make compose-down
```

#### 单容器构建与运行
```bash
# 构建多阶段轻量镜像
make image IMAGE=echoa TAG=latest

# 运行容器
docker run --rm -p 8080:8080 \
  -e MCP_RESOURCE_URL=http://localhost:8080 \
  -e MYSQL_DSN='root:password@tcp(host.docker.internal:3306)/test?charset=utf8mb4&parseTime=True&loc=Local&multiStatements=false' \
  -e REDIS_ADDR=host.docker.internal:6379 \
  echoa:latest
```

### 3. 发布镜像

[.github/workflows/image.yml](.github/workflows/image.yml) 在推送到 `main` 或打 `v*` tag 时构建 `linux/amd64` 与 `linux/arm64` 镜像并推送。

- **ghcr.io** 默认开启，使用仓库自带的 `GITHUB_TOKEN`，无需额外配置。推到 `main` 得到 `ghcr.io/<owner>/<repo>:latest` 和 `:sha-<短 SHA>`；打 tag `v1.2.0` 得到 `:1.2.0` 和 `:v1.2.0`。首次推送后镜像默认是私有的，到仓库 Packages 页面改为 Public 才能匿名拉取。
- **Docker Hub** 默认关闭。在仓库 Settings → Secrets and variables → Actions 添加 `DOCKERHUB_USERNAME` 和 `DOCKERHUB_TOKEN`（Access Token，不用账号密码），再把 workflow 里的 `env.dockerhub` 改成 `"true"`。
- 在 Actions 页面手动触发 `image` 只构建不推送，用来先确认 Dockerfile 能过。

### 4. 运行测试
```bash
make test
# 或
go test -v ./...
```

---

## 客户端接入示例

在 AI 编辑器（Cursor / Claude Desktop / LibreChat 等）中添加 MCP 配置：

```json
{
  "mcpServers": {
    "echoa": {
      "type": "sse",
      "url": "http://127.0.0.1:8080/sse",
      "headers": {
        "Authorization": "Bearer <YOUR_CASDOOR_JWT_TOKEN>"
      }
    }
  }
}
```

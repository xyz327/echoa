# ==========================================
# 第一阶段：编译构建阶段 (Builder)
# ==========================================
FROM golang:alpine AS builder

WORKDIR /app

# 安装必要的依赖与证书
RUN apk add --no-cache git ca-certificates tzdata

# 利用 Docker 缓存层优化依赖下载
COPY go.mod go.sum ./
RUN go mod download

# 复制源码
COPY . .

# 静态编译二进制可执行文件
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w" \
    -buildvcs=false \
    -o /app/bin/server ./cmd/server

# ==========================================
# 第二阶段：生产运行镜像 (Minimal Runtime)
# ==========================================
FROM alpine:3.21

# 安装 CA 根证书与 curl（供健康检查探针使用），并创建非 root 用户
RUN apk add --no-cache ca-certificates tzdata curl && \
    addgroup -S appgroup && adduser -S appuser -G appgroup

WORKDIR /app

# 从 builder 阶段复制可执行文件与默认配置文件
COPY --from=builder /app/bin/server /app/server
COPY --from=builder /app/config/config.yaml /app/config/config.yaml

# 赋予非 root 账号最小运行权限
RUN chown -R appuser:appgroup /app
USER appuser

# 暴露 MCP 端口
EXPOSE 8080

# 原生容器存活健康探针（每 15 秒探活一次）
HEALTHCHECK --interval=15s --timeout=3s --start-period=5s --retries=3 \
  CMD curl -f http://127.0.0.1:8080/health || exit 1

# 启动服务
ENTRYPOINT ["/app/server"]
CMD ["-config", "/app/config/config.yaml"]

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"echoa/internal/auth"
	"echoa/internal/config"
	"echoa/internal/mcp"
	"echoa/internal/modules/mysql"
	"echoa/internal/modules/redis"
	"github.com/casdoor/casdoor-go-sdk/casdoorsdk"
)

func main() {
	configPath := flag.String("config", "config/config.yaml", "path to config file")
	flag.Parse()

	// 1. 加载配置
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("[FATAL] Failed to load config from '%s': %v", *configPath, err)
	}

	log.Printf("[INFO] Config loaded successfully (Auth enabled: %v, ResourceURL: %s)",
		cfg.Auth.Enabled, cfg.Auth.ResourceURL)

	// 2. 初始化 Casdoor SDK Client
	var casdoorClient *casdoorsdk.Client
	if cfg.Auth.Enabled {
		casdoorClient = casdoorsdk.NewClient(
			cfg.Auth.Endpoint,
			cfg.Auth.ClientID,
			cfg.Auth.ClientSecret,
			cfg.Auth.Certificate,
			cfg.Auth.OrganizationName,
			cfg.Auth.ApplicationName,
		)
		log.Printf("[INFO] Casdoor client initialized for endpoint: %s", cfg.Auth.Endpoint)
	}

	// 3. 初始化 MCP 工具注册中心并挂载数据源
	registry := mcp.NewToolRegistry()

	var mysqlModule *mysql.Module
	if cfg.DataSources.MySQL.Enabled {
		mod, err := mysql.Register(registry, cfg.DataSources.MySQL)
		if err != nil {
			log.Fatalf("[FATAL] Failed to register MySQL module: %v", err)
		}
		mysqlModule = mod
		log.Printf("[INFO] MySQL module registered (max_limit: %d)", cfg.DataSources.MySQL.Security.MaxLimit)
	}

	var redisModule *redis.Module
	if cfg.DataSources.Redis.Enabled {
		mod, err := redis.Register(registry, cfg.DataSources.Redis)
		if err != nil {
			log.Fatalf("[FATAL] Failed to register Redis module: %v", err)
		}
		redisModule = mod
		log.Printf("[INFO] Redis module registered (allowed_dbs: %v)", cfg.DataSources.Redis.AllowedDBs)
	}

	// 打印已注册的工具列表
	for _, tool := range registry.ListTools() {
		log.Printf("  -> [MCP TOOL] %s: %s", tool.Name, tool.Description)
	}

	mcpServer := mcp.NewServer(registry, cfg.Auth.ResourceURL)

	// 4. 组装路由
	mux := http.NewServeMux()


	// 5. 健康检查接口 (支持 Liveness 与 Readiness 探活)
	checkReadiness := func(w http.ResponseWriter, r *http.Request) {
		status := map[string]any{}
		allHealthy := true

		if cfg.DataSources.MySQL.Enabled {
			pingCtx, cancel := context.WithTimeout(r.Context(), 1*time.Second)
			defer cancel()
			if err := mysqlModule.Ping(pingCtx); err != nil {
				status["mysql"] = "error: " + err.Error()
				allHealthy = false
			} else {
				status["mysql"] = "ok"
			}
		}

		if cfg.DataSources.Redis.Enabled {
			pingCtx, cancel := context.WithTimeout(r.Context(), 1*time.Second)
			defer cancel()
			if err := redisModule.Ping(pingCtx); err != nil {
				status["redis"] = "error: " + err.Error()
				allHealthy = false
			} else {
				status["redis"] = "ok"
			}
		}

		statusCode := http.StatusOK
		respStatus := "ok"
		if !allHealthy {
			statusCode = http.StatusServiceUnavailable
			respStatus = "degraded"
		}

		writeJSON(w, statusCode, map[string]any{
			"status":      respStatus,
			"service":     "echoa",
			"datasources": status,
		})
	}

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("type") == "ready" {
			checkReadiness(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "echoa"})
	})

	mux.HandleFunc("GET /health/ready", checkReadiness)

	// RFC 9728 保护资源元数据接口（MCP OAuth 规范，供 Cursor / Claude 等客户端自动发现 Casdoor）
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", auth.ProtectedResourceMetadataHandler(cfg.Auth))

	// Casdoor 鉴权中间件
	authMw := auth.CasdoorMiddleware(cfg.Auth, casdoorClient)

	// 注册 MCP 协议规定的 SSE 与 Message 路由
	mux.Handle("GET /sse", authMw(http.HandlerFunc(mcpServer.HandleSSE)))
	mux.Handle("POST /message", authMw(http.HandlerFunc(mcpServer.HandleMessage)))

	readTimeout := time.Duration(cfg.Server.ReadTimeoutMS) * time.Millisecond
	srv := &http.Server{
		Addr:        fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:     mux,
		ReadTimeout: readTimeout,
		// WriteTimeout 不设置：SSE 长连接由会话空闲超时（15 分钟）回收，
		// 而 WriteTimeout 从请求头读完起算，会把正常的长连接提前掐断。
		IdleTimeout: 60 * time.Second,
	}

	// 5. 优雅停机：收到信号后停止接收新请求，再释放连接池与 MCP 会话
	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
		<-stop

		log.Println("[INFO] Graceful shutdown triggered, releasing resources...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("[WARN] HTTP server shutdown: %v", err)
		}
	}()

	log.Printf("[INFO] Server listening on http://0.0.0.0:%d", cfg.Server.Port)
	log.Printf("[INFO] MCP PRM endpoint: GET http://0.0.0.0:%d/.well-known/oauth-protected-resource", cfg.Server.Port)
	log.Printf("[INFO] MCP SSE endpoint: GET http://0.0.0.0:%d/sse", cfg.Server.Port)
	log.Printf("[INFO] MCP Message endpoint: POST http://0.0.0.0:%d/message", cfg.Server.Port)

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[FATAL] HTTP server exited: %v", err)
	}

	if mcpServer != nil {
		mcpServer.Close()
	}
	if mysqlModule != nil {
		_ = mysqlModule.Close()
		log.Println("[INFO] MySQL connection pool closed")
	}
	if redisModule != nil {
		_ = redisModule.Close()
		log.Println("[INFO] Redis connection pools closed")
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

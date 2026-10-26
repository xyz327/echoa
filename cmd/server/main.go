package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/casdoor/casdoor-go-sdk/casdoorsdk"
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/utils"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"echoa/internal/auth"
	"echoa/internal/config"
	"echoa/internal/mcp"
	"echoa/internal/modules/mysql"
	"echoa/internal/modules/redis"
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

	// 4. 初始化 Hertz HTTP 引擎
	h := server.Default(
		server.WithHostPorts(fmt.Sprintf(":%d", cfg.Server.Port)),
	)

	// 注册优雅停机钩子（释放连接池与 MCP 会话）
	h.OnShutdown = append(h.OnShutdown, func(ctx context.Context) {
		log.Println("[INFO] Graceful shutdown triggered, releasing resources...")
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
	})

	// 5. 健康检查接口 (支持 Liveness 与 Readiness 探活)
	checkReadiness := func(ctx context.Context, c *app.RequestContext) {
		status := utils.H{}
		allHealthy := true

		if cfg.DataSources.MySQL.Enabled {
			pingCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
			defer cancel()
			if err := mysqlModule.Ping(pingCtx); err != nil {
				status["mysql"] = "error: " + err.Error()
				allHealthy = false
			} else {
				status["mysql"] = "ok"
			}
		}

		if cfg.DataSources.Redis.Enabled {
			pingCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
			defer cancel()
			if err := redisModule.Ping(pingCtx); err != nil {
				status["redis"] = "error: " + err.Error()
				allHealthy = false
			} else {
				status["redis"] = "ok"
			}
		}

		statusCode := consts.StatusOK
		respStatus := "ok"
		if !allHealthy {
			statusCode = consts.StatusServiceUnavailable
			respStatus = "degraded"
		}

		c.JSON(statusCode, utils.H{
			"status":      respStatus,
			"service":     "echoa",
			"datasources": status,
		})
	}

	h.GET("/health", func(ctx context.Context, c *app.RequestContext) {
		if string(c.Query("type")) == "ready" {
			checkReadiness(ctx, c)
			return
		}
		c.JSON(consts.StatusOK, utils.H{"status": "ok", "service": "echoa"})
	})

	h.GET("/health/ready", func(ctx context.Context, c *app.RequestContext) {
		checkReadiness(ctx, c)
	})

	// RFC 9728 保护资源元数据接口（MCP OAuth 规范，供 Cursor / Claude 等客户端自动发现 Casdoor）
	h.GET("/.well-known/oauth-protected-resource", auth.ProtectedResourceMetadataHandler(cfg.Auth))

	// Casdoor 鉴权中间件
	authMw := auth.CasdoorMiddleware(cfg.Auth, casdoorClient)

	// 注册 MCP 协议规定的 SSE 与 Message 路由
	h.GET("/sse", authMw, mcpServer.HandleSSE)
	h.POST("/message", authMw, mcpServer.HandleMessage)

	log.Printf("[INFO] Server listening on http://0.0.0.0:%d", cfg.Server.Port)
	log.Printf("[INFO] MCP PRM endpoint: GET http://0.0.0.0:%d/.well-known/oauth-protected-resource", cfg.Server.Port)
	log.Printf("[INFO] MCP SSE endpoint: GET http://0.0.0.0:%d/sse", cfg.Server.Port)
	log.Printf("[INFO] MCP Message endpoint: POST http://0.0.0.0:%d/message", cfg.Server.Port)

	h.Spin()
}

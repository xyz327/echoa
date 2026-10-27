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

// pinger 是健康检查探活的最小接口，MySQL 与 Redis 模块都实现了它。
type pinger interface {
	Ping(ctx context.Context) error
	Close() error
}

// runningSource 按种类留着具体模块，复用连接池时不需要从接口再断言回去。
type runningSource struct {
	name  string
	kind  string
	mysql *mysql.Module
	redis *redis.Module
}

func (s *runningSource) conn() pinger {
	if s.mysql != nil {
		return s.mysql
	}
	return s.redis
}

type runningEndpoint struct {
	server *mcp.Server
	path   string
}

func main() {
	configPath := flag.String("config", "config/config.yaml", "path to config file")
	flag.Parse()

	// 1. 加载配置
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("[FATAL] Failed to load config from '%s': %v", *configPath, err)
	}

	log.Printf("[INFO] Config loaded successfully (Auth enabled: %v, ResourceURL: %s, endpoints: %d)",
		cfg.Auth.Enabled, cfg.Auth.ResourceURL, len(cfg.Endpoints))

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

	// 3. 每个 endpoint 一个 MCP Server 和独立的工具表。
	//    同一个 source 被多个 endpoint 引用时只开一个连接池，工具注册进各自的 registry。
	sources := map[string]*runningSource{}
	var endpoints []runningEndpoint

	closeSources := func() {
		for _, src := range sources {
			if conn := src.conn(); conn != nil {
				_ = conn.Close()
			}
		}
	}

	for _, ep := range cfg.Endpoints {
		registry := mcp.NewToolRegistry()
		kindCount := map[string]int{}
		for _, name := range ep.Sources {
			ref, _ := cfg.Lookup(name)
			kindCount[ref.Kind]++
		}

		for _, name := range ep.Sources {
			ref, _ := cfg.Lookup(name)
			qualify := kindCount[ref.Kind] > 1

			if existing, ok := sources[name]; ok {
				switch ref.Kind {
				case config.SourceKindMySQL:
					mysql.RegisterExisting(registry, name, existing.mysql, qualify)
				case config.SourceKindRedis:
					redis.RegisterExisting(registry, name, existing.redis, qualify)
				}
				log.Printf("[INFO] Endpoint %s reuses source %q (%s)", ep.Path, name, ref.Kind)
				continue
			}

			running := &runningSource{name: name, kind: ref.Kind}
			switch ref.Kind {
			case config.SourceKindMySQL:
				m, regErr := mysql.Register(registry, name, ref.MySQL, qualify)
				if regErr != nil {
					closeSources()
					log.Fatalf("[FATAL] Failed to register MySQL source %q: %v", name, regErr)
				}
				running.mysql = m
				log.Printf("[INFO] MySQL source %q registered on %s (max_limit: %d, qualified: %v)",
					name, ep.Path, ref.MySQL.Security.MaxLimit, qualify)
			case config.SourceKindRedis:
				m, regErr := redis.Register(registry, name, ref.Redis, qualify)
				if regErr != nil {
					closeSources()
					log.Fatalf("[FATAL] Failed to register Redis source %q: %v", name, regErr)
				}
				running.redis = m
				log.Printf("[INFO] Redis source %q registered on %s (allowed_dbs: %v, qualified: %v)",
					name, ep.Path, ref.Redis.AllowedDBs, qualify)
			default:
				closeSources()
				log.Fatalf("[FATAL] Endpoint %s references source %q of unknown kind %q", ep.Path, name, ref.Kind)
			}
			sources[name] = running
		}

		for _, tool := range registry.ListTools() {
			log.Printf("  -> [MCP TOOL %s] %s: %s", ep.Path, tool.Name, tool.Description)
		}

		srv := mcp.NewServer(ep.Path, registry, cfg.Server.MaxInflightPerPrincipal, cfg.Server.AllowedOrigins)
		endpoints = append(endpoints, runningEndpoint{server: srv, path: ep.Path})
	}

	// 4. 组装路由
	mux := http.NewServeMux()

	// 5. 健康检查接口 (支持 Liveness 与 Readiness 探活)
	checkReadiness := func(w http.ResponseWriter, r *http.Request) {
		status := map[string]any{}
		allHealthy := true

		for _, src := range sources {
			key := src.kind + ":" + src.name
			pingCtx, cancel := context.WithTimeout(r.Context(), 1*time.Second)
			err := src.conn().Ping(pingCtx)
			cancel()
			if err != nil {
				status[key] = "error: " + err.Error()
				allHealthy = false
			} else {
				status[key] = "ok"
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

	// 每个 endpoint 挂独立的 MCP 2026-07-28 POST 路由
	for _, ep := range endpoints {
		mux.Handle(ep.path, authMw(ep.server))
	}

	readTimeout := time.Duration(cfg.Server.ReadTimeoutMS) * time.Millisecond
	srv := &http.Server{
		Addr:        fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:     mux,
		ReadTimeout: readTimeout,
		// WriteTimeout 不设置：单次响应虽不是长连接，但耗时查询（如复杂 AST 校验及大型查询）由各自的 query_timeout_ms 控制。
		IdleTimeout: 60 * time.Second,
	}

	// 6. 优雅停机：收到信号后停止接收新请求，再释放连接池
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
	for _, ep := range endpoints {
		log.Printf("[INFO] MCP endpoint %s: POST %s", ep.path, ep.path)
	}

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[FATAL] HTTP server exited: %v", err)
	}

	for _, ep := range endpoints {
		ep.server.Close()
	}
	for _, src := range sources {
		if conn := src.conn(); conn != nil {
			_ = conn.Close()
			log.Printf("[INFO] %s source %q connection pool closed", src.kind, src.name)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

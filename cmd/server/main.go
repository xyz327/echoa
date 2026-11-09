package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"echoa/internal/auth"
	"echoa/internal/config"
	"echoa/internal/datasource"
	"echoa/internal/logger"
	"echoa/internal/mcp"
	_ "echoa/internal/modules/mysql"
	_ "echoa/internal/modules/redis"
	"github.com/casdoor/casdoor-go-sdk/casdoorsdk"
)

type runningEndpoint struct {
	server *mcp.Server
	path   string
}

func main() {
	configPath := flag.String("config", "config/config.yaml", "path to config file")
	logFormat := flag.String("log-format", "json", "log format: json or text")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn, error")
	flag.Parse()

	// 1. 初始化统一结构化日志
	logger.Init(*logLevel, *logFormat)

	// 2. 加载配置
	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("Failed to load config", slog.String("path", *configPath), slog.Any("error", err))
		os.Exit(1)
	}

	slog.Info("Config loaded successfully",
		slog.Bool("auth_enabled", cfg.Auth.Enabled),
		slog.String("resource_url", cfg.Auth.ResourceURL),
		slog.Int("endpoints_count", len(cfg.Endpoints)),
	)

	// 3. 初始化 Casdoor SDK Client
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
		slog.Info("Casdoor client initialized", slog.String("endpoint", cfg.Auth.Endpoint))
	}

	// 4. 每个 endpoint 一个 MCP Server 和独立的工具表。
	//    同一个 source 被多个 endpoint 引用时复用同一个 Instance，工具注册进各自的 registry。
	sources := map[string]datasource.Instance{}
	var endpoints []runningEndpoint

	closeSources := func() {
		for _, inst := range sources {
			if inst != nil {
				_ = inst.Close()
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
				existing.RegisterTools(registry, name, qualify)
				slog.Info("Endpoint reuses source", slog.String("endpoint", ep.Path), slog.String("source", name), slog.String("kind", ref.Kind))
				continue
			}

			inst, regErr := datasource.Create(ref)
			if regErr != nil {
				closeSources()
				slog.Error("Failed to create datasource", slog.String("source", name), slog.String("kind", ref.Kind), slog.Any("error", regErr))
				os.Exit(1)
			}

			inst.RegisterTools(registry, name, qualify)
			sources[name] = inst
			slog.Info("Source registered", slog.String("endpoint", ep.Path), slog.String("source", name), slog.String("kind", ref.Kind), slog.Bool("qualified", qualify))
		}

		for _, tool := range registry.ListTools() {
			slog.Debug("MCP tool registered", slog.String("endpoint", ep.Path), slog.String("tool", tool.Name))
		}

		srv := mcp.NewServer(ep.Path, registry, cfg.Server.MaxInflightPerPrincipal, cfg.Server.AllowedOrigins)
		endpoints = append(endpoints, runningEndpoint{server: srv, path: ep.Path})
	}

	// 5. 组装路由
	mux := http.NewServeMux()

	// 6. 健康检查接口 (支持 Liveness 与 Readiness 探活)
	checkReadiness := func(w http.ResponseWriter, r *http.Request) {
		status := map[string]any{}
		allHealthy := true

		for name, inst := range sources {
			pingCtx, cancel := context.WithTimeout(r.Context(), 1*time.Second)
			err := inst.Ping(pingCtx)
			cancel()
			if err != nil {
				status[name] = "error: " + err.Error()
				allHealthy = false
			} else {
				status[name] = "ok"
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

	// RFC 9728 保护资源元数据接口
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", auth.ProtectedResourceMetadataHandler(cfg.Auth))

	// Casdoor 鉴权中间件
	authMw := auth.CasdoorMiddleware(cfg.Auth, casdoorClient)

	// 每个 endpoint 挂独立的 MCP POST 路由
	for _, ep := range endpoints {
		mux.Handle(ep.path, authMw(ep.server))
	}

	readTimeout := time.Duration(cfg.Server.ReadTimeoutMS) * time.Millisecond
	srv := &http.Server{
		Addr:        fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:     mux,
		ReadTimeout: readTimeout,
		IdleTimeout: 60 * time.Second,
	}

	// 7. 优雅停机
	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
		<-stop

		slog.Info("Graceful shutdown triggered, releasing resources...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("HTTP server shutdown warning", slog.Any("error", err))
		}
	}()

	slog.Info("Server listening", slog.String("addr", fmt.Sprintf("http://0.0.0.0:%d", cfg.Server.Port)))
	slog.Info("MCP PRM endpoint", slog.String("url", fmt.Sprintf("http://0.0.0.0:%d/.well-known/oauth-protected-resource", cfg.Server.Port)))
	for _, ep := range endpoints {
		slog.Info("MCP endpoint registered", slog.String("path", ep.path))
	}

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("HTTP server exited with error", slog.Any("error", err))
		os.Exit(1)
	}

	for _, ep := range endpoints {
		ep.server.Close()
	}
	for name, inst := range sources {
		if inst != nil {
			_ = inst.Close()
			slog.Info("Datasource closed", slog.String("source", name))
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

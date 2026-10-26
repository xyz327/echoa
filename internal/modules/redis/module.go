package redis

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"echoa/internal/config"
	"echoa/internal/mcp"
)

type Module struct {
	cfg        config.RedisConfig
	dbMgr      *MultiDBManager
	allowedMap map[string]bool
	blockedMap map[string]bool
}

func (m *Module) Close() error {
	if m != nil && m.dbMgr != nil {
		return m.dbMgr.Close()
	}
	return nil
}

func (m *Module) Ping(ctx context.Context) error {
	if m == nil || m.dbMgr == nil {
		return fmt.Errorf("redis module not initialized")
	}
	client, err := m.dbMgr.GetClient(0)
	if err != nil {
		return err
	}
	return client.Ping(ctx).Err()
}

func Register(registry *mcp.ToolRegistry, cfg config.RedisConfig) (*Module, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	allowedMap := make(map[string]bool)
	for _, cmd := range cfg.Security.AllowedCommands {
		allowedMap[strings.ToUpper(strings.TrimSpace(cmd))] = true
	}

	blockedMap := make(map[string]bool)
	for _, cmd := range cfg.Security.BlockedCommands {
		blockedMap[strings.ToUpper(strings.TrimSpace(cmd))] = true
	}

	baseOpts := &redis.Options{
		Addr:            cfg.Addr,
		Password:        cfg.Password,
		PoolSize:        cfg.PoolSize,
		MinIdleConns:    cfg.MinIdleConns,
		ConnMaxIdleTime: 10 * time.Minute,
		ConnMaxLifetime: 1 * time.Hour,
	}

	m := &Module{
		cfg:        cfg,
		dbMgr:      NewMultiDBManager(baseOpts, cfg.AllowedDBs),
		allowedMap: allowedMap,
		blockedMap: blockedMap,
	}

	// 启动探活（超时 3 秒），失败打印警告日志以兼顾本地无 DB 开发
	client, err := m.dbMgr.GetClient(0)
	if err == nil {
		pingCtx, pingCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer pingCancel()
		if pingErr := client.Ping(pingCtx).Err(); pingErr != nil {
			log.Printf("[WARN] Failed to ping Redis (%s): %v (server continues to run)", cfg.Addr, pingErr)
		} else {
			log.Printf("[INFO] Redis connection ping successful")
		}
	}

	// 1. 注册统一命令网关 redis_execute
	registry.Register(&mcp.ToolDefinition{
		Name: "redis_execute",
		Description: "Execute a read-only Redis command (e.g. GET, HGETALL, LRANGE, SMEMBERS, ZRANGE, TYPE, TTL). " +
			"Supports optional 'db' argument (0-15, default 0). Dangerous/write commands are strictly forbidden.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "Redis command name (e.g. 'GET', 'HGETALL', 'LRANGE')",
				},
				"args": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Arguments for the command. Example: ['user:profile:1001']",
				},
				"db": map[string]any{
					"type":        "integer",
					"description": "Optional logical database index (default 0)",
					"default":     0,
				},
			},
			"required": []string{"command", "args"},
		},
		Handler: m.HandleExecute,
	})

	// 2. 注册安全扫描工具 redis_scan_keys
	registry.Register(&mcp.ToolDefinition{
		Name: "redis_scan_keys",
		Description: "Safely search keys matching a glob pattern using SCAN without blocking the server. " +
			"Supports optional 'db' parameter.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "Glob pattern to match keys (e.g. 'user:*', '*cache*')",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Max number of keys to return (default 50)",
					"default":     50,
				},
				"db": map[string]any{
					"type":        "integer",
					"description": "Optional logical database index (default 0)",
					"default":     0,
				},
			},
			"required": []string{"pattern"},
		},
		Handler: m.HandleScanKeys,
	})

	// 3. 注册数据库分布查看工具 redis_list_dbs
	registry.Register(&mcp.ToolDefinition{
		Name:        "redis_list_dbs",
		Description: "List all Redis databases that currently hold keys and their approximate key counts.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Handler: m.HandleListDBs,
	})

	return m, nil
}

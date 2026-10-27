package redis

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"echoa/internal/config"
	"echoa/internal/mcp"
	"github.com/redis/go-redis/v9"
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

// Register 把一份 Redis 配置注册成一组只读工具。
// qualify 为 false 时工具名是 redis_execute；同一个 endpoint 挂多个 Redis 时带上 source 名字。
func Register(registry *mcp.ToolRegistry, name string, cfg config.RedisConfig, qualify bool) (*Module, error) {
	return registerModule(registry, name, cfg, qualify, nil)
}

// RegisterExisting 把已经建好连接池的模块再注册到另一个 endpoint 的工具表，不再新开连接。
func RegisterExisting(registry *mcp.ToolRegistry, name string, m *Module, qualify bool) {
	_, _ = registerModule(registry, name, m.cfg, qualify, m)
}

func registerModule(registry *mcp.ToolRegistry, name string, cfg config.RedisConfig, qualify bool, existing *Module) (*Module, error) {
	if existing != nil {
		registerTools(registry, name, existing, qualify)
		return existing, nil
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
		Username:        cfg.Username,
		Password:        cfg.Password,
		PoolSize:        cfg.PoolSize,
		MinIdleConns:    cfg.MinIdleConns,
		ConnMaxIdleTime: 10 * time.Minute,
		ConnMaxLifetime: 1 * time.Hour,
		// min_idle_conns 会在连接池创建时后台预热拨号。go-redis v9 的 dial 默认重试 5 次，
		// 而且 <= 0 会被当成「用默认值」，所以想只拨一次必须设 1。
		// Redis 不在时否则每个空闲连接都要白等一轮。命令重试（MaxRetries）保持默认。
		DialerRetries: 1,
	}

	m := &Module{
		cfg:        cfg,
		dbMgr:      NewMultiDBManager(baseOpts, cfg.AllowedDBs),
		allowedMap: allowedMap,
		blockedMap: blockedMap,
	}

	pingRedis(m, cfg.Addr)
	registerTools(registry, name, m, qualify)
	return m, nil
}

func pingRedis(m *Module, addr string) {
	// 启动探活（超时 3 秒），失败打印警告日志以兼顾本地无 DB 开发。
	// MaxRetries 设 -1 表示不重试：go-redis 把 0 当成「用默认的 3 次」。
	// 连接池的 dial 重试已经在上面收成一次，这里再关掉命令重试，
	// 避免本地没起 Redis 时探活被拖住。
	opts := *m.dbMgr.BaseOptions()
	opts.DB = 0
	opts.MaxRetries = -1
	client := redis.NewClient(&opts)
	defer client.Close()
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer pingCancel()
	if pingErr := client.Ping(pingCtx).Err(); pingErr != nil {
		log.Printf("[WARN] Failed to ping Redis (%s): %v (server continues to run)", addr, pingErr)
	} else {
		log.Printf("[INFO] Redis connection ping successful")
	}
}

func registerTools(registry *mcp.ToolRegistry, name string, m *Module, qualify bool) {
	prefix := "redis"
	if qualify {
		prefix = name + "_redis"
	}

	// 1. 注册统一命令网关 redis_execute
	registry.Register(&mcp.ToolDefinition{
		Name: prefix + "_execute",
		Description: "Execute a read-only Redis command (e.g. GET, HGETALL, LRANGE, SMEMBERS, ZRANGE, TYPE, TTL). " +
			"Supports optional 'db' argument (0-15, default 0). Dangerous/write commands are strictly forbidden. Source: " + name + ".",
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
		Name: prefix + "_scan_keys",
		Description: "Safely search keys matching a glob pattern using SCAN without blocking the server. " +
			"Supports optional 'db' parameter. Source: " + name + ".",
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
		Name:        prefix + "_list_dbs",
		Description: "List all Redis databases that currently hold keys and their approximate key counts. Source: " + name + ".",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Handler: m.HandleListDBs,
	})
}

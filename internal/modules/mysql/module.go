package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"echoa/internal/config"
	"echoa/internal/mcp"
	_ "github.com/go-sql-driver/mysql"
)

type Module struct {
	cfg config.MySQLConfig
	db  *sql.DB
}

func (m *Module) Close() error {
	if m != nil && m.db != nil {
		return m.db.Close()
	}
	return nil
}

func (m *Module) Ping(ctx context.Context) error {
	if m == nil || m.db == nil {
		return fmt.Errorf("mysql module not initialized")
	}
	return m.db.PingContext(ctx)
}

// Register 把一份 MySQL 配置注册成一组只读工具。
// name 是 source 名字：一个 endpoint 只挂这一个 MySQL 时工具名保持 mysql_query；
// 同一个 endpoint 挂多个 MySQL 时，工具名带上名字（orders_mysql_query），避免互相覆盖。
func Register(registry *mcp.ToolRegistry, name string, cfg config.MySQLConfig, qualify bool) (*Module, error) {
	return registerModule(registry, name, cfg, qualify, nil)
}

// RegisterExisting 把已经建好连接池的模块再注册到另一个 endpoint 的工具表，不再新开连接。
func RegisterExisting(registry *mcp.ToolRegistry, name string, m *Module, qualify bool) {
	_, _ = registerModule(registry, name, m.cfg, qualify, m)
}

func registerModule(registry *mcp.ToolRegistry, name string, cfg config.MySQLConfig, qualify bool, existing *Module) (*Module, error) {
	if existing != nil {
		registerTools(registry, name, existing, qualify)
		return existing, nil
	}

	db, err := sql.Open("mysql", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("failed to open mysql connection: %w", err)
	}

	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	// 连接超过存活期会被回收重建，空闲过久的连接也会被主动关闭，
	// 避免被 MySQL wait_timeout 或 SLB 静默掐断后出现 driver: bad connection。
	db.SetConnMaxLifetime(time.Hour)
	db.SetConnMaxIdleTime(10 * time.Minute)

	// 启动探活（超时 3 秒），失败打印警告日志以兼顾本地无 DB 开发
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer pingCancel()
	if err := db.PingContext(pingCtx); err != nil {
		log.Printf("[WARN] Failed to ping MySQL (%s): %v (server continues to run)", cfg.DSN, err)
	} else {
		log.Printf("[INFO] MySQL connection ping successful")
	}

	m := &Module{
		cfg: cfg,
		db:  db,
	}
	registerTools(registry, name, m, qualify)
	return m, nil
}

func registerTools(registry *mcp.ToolRegistry, name string, m *Module, qualify bool) {
	prefix := "mysql"
	if qualify {
		prefix = name + "_mysql"
	}

	// 1. 注册 mysql_list_tables
	registry.Register(&mcp.ToolDefinition{
		Name: prefix + "_list_tables",
		Description: "List all accessible tables in the current MySQL database with row estimates and metadata. " +
			"Filtered against table blacklist. Source: " + name + ".",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Handler: m.HandleListTables,
	})

	// 2. 注册 mysql_describe_table
	registry.Register(&mcp.ToolDefinition{
		Name:        prefix + "_describe_table",
		Description: "Get column names, types, nullability, keys, default values, and comments for a specified MySQL table. Source: " + name + ".",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"table_name": map[string]any{
					"type":        "string",
					"description": "Name of the table to describe",
				},
			},
			"required": []string{"table_name"},
		},
		Handler: m.HandleDescribeTable,
	})

	// 3. 注册 mysql_query
	registry.Register(&mcp.ToolDefinition{
		Name: prefix + "_query",
		Description: "Execute a read-only MySQL SQL query (SELECT, EXPLAIN, SHOW). " +
			"Enforces strict AST verification, table blacklist checks, and automatic LIMIT capping. Source: " + name + ".",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "SQL statement to execute (e.g. SELECT * FROM users WHERE status = 1)",
				},
				"max_rows": map[string]any{
					"type":        "integer",
					"description": "Optional row limit override (cannot exceed configured max_limit)",
				},
			},
			"required": []string{"query"},
		},
		Handler: m.HandleQuery,
	})
}

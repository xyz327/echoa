package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"echoa/internal/config"
	"echoa/internal/datasource"
	"echoa/internal/mcp"
	_ "github.com/go-sql-driver/mysql"
)

func init() {
	datasource.Register(config.SourceKindMySQL, func(ref config.SourceRef) (datasource.Instance, error) {
		return Open(ref.Name, ref.MySQL)
	})
}

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

// RegisterTools 满足 datasource.Instance 接口，向 ToolRegistry 注册 MySQL 对应工具
func (m *Module) RegisterTools(registry *mcp.ToolRegistry, sourceName string, qualify bool) {
	registerTools(registry, sourceName, m, qualify)
}

// Open 初始化 MySQL 连接池并执行启动探活
func Open(name string, cfg config.MySQLConfig) (*Module, error) {
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
		slog.Warn("Failed to ping MySQL (server continues to run)", slog.String("source", name), slog.String("dsn", cfg.DSN), slog.Any("error", err))
	} else {
		slog.Info("MySQL connection ping successful", slog.String("source", name))
	}

	return &Module{
		cfg: cfg,
		db:  db,
	}, nil
}

// Register 把一份 MySQL 配置注册成一组只读工具。
func Register(registry *mcp.ToolRegistry, name string, cfg config.MySQLConfig, qualify bool) (*Module, error) {
	m, err := Open(name, cfg)
	if err != nil {
		return nil, err
	}
	m.RegisterTools(registry, name, qualify)
	return m, nil
}

// RegisterExisting 把已经建好连接池的模块再注册到另一个 endpoint 的工具表，不再新开连接。
func RegisterExisting(registry *mcp.ToolRegistry, name string, m *Module, qualify bool) {
	m.RegisterTools(registry, name, qualify)
}

func registerTools(registry *mcp.ToolRegistry, name string, m *Module, qualify bool) {
	prefix := "mysql"
	if qualify {
		prefix = name + "_mysql"
	}

	// 1. 注册 mysql_list_databases
	registry.Register(&mcp.ToolDefinition{
		Name: prefix + "_list_databases",
		Description: "List all accessible databases/schemas in the MySQL instance. " +
			"Returns database names, charset, collation, and current database. Source: " + name + ".",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "Optional SQL LIKE pattern to filter database names (e.g. 'order_%')",
				},
			},
		},
		Handler: m.HandleListDatabases,
	})

	// 2. 注册 mysql_list_tables
	registry.Register(&mcp.ToolDefinition{
		Name: prefix + "_list_tables",
		Description: "List accessible tables with row estimates and metadata. " +
			"Filtered against table blacklist. Supports querying a specific schema or listing across databases. Source: " + name + ".",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"schema": map[string]any{
					"type":        "string",
					"description": "Optional database/schema name to list tables from. If omitted, uses current database or lists across accessible user databases.",
				},
			},
		},
		Handler: m.HandleListTables,
	})

	// 3. 注册 mysql_describe_table
	registry.Register(&mcp.ToolDefinition{
		Name: prefix + "_describe_table",
		Description: "Get column names, types, nullability, keys, default values, and comments for a specified MySQL table. " +
			"Supports 'table' or 'schema.table' (e.g. 'users' or 'billing.orders'). Source: " + name + ".",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"table_name": map[string]any{
					"type":        "string",
					"description": "Name of the table to describe. Supports 'table' or 'schema.table' (e.g. 'users' or 'billing.orders')",
				},
				"schema": map[string]any{
					"type":        "string",
					"description": "Optional database/schema name. If table_name contains a dot, this parameter is ignored.",
				},
			},
			"required": []string{"table_name"},
		},
		Handler: m.HandleDescribeTable,
	})

	// 4. 注册 mysql_query
	registry.Register(&mcp.ToolDefinition{
		Name: prefix + "_query",
		Description: "Execute a read-only MySQL SQL query (SELECT, EXPLAIN, SHOW). " +
			"Enforces strict AST verification, table blacklist checks, and automatic LIMIT capping. " +
			"Supports cross-database queries (e.g. SELECT * FROM other_db.table) and optional default schema. Source: " + name + ".",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "SQL statement to execute (e.g. SELECT * FROM users, or cross-db: SELECT * FROM other_db.orders)",
				},
				"schema": map[string]any{
					"type":        "string",
					"description": "Optional default database/schema name for unqualified table names in the query",
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

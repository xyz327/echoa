package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/casdoor/casdoor-go-sdk/casdoorsdk"
)

// HandleListDatabases 对应工具 mysql_list_databases，列出当前 MySQL 实例上的所有可用业务数据库/schema
func (m *Module) HandleListDatabases(ctx context.Context, claims *casdoorsdk.Claims, args map[string]any) (string, error) {
	queryCtx, cancel := context.WithTimeout(ctx, time.Duration(m.cfg.QueryTimeoutMS)*time.Millisecond)
	defer cancel()

	pattern, _ := args["pattern"].(string)
	pattern = strings.TrimSpace(pattern)

	var sqlStr string
	var queryArgs []any

	if pattern != "" {
		sqlStr = `
			SELECT schema_name, default_character_set_name, default_collation_name
			FROM information_schema.schemata
			WHERE schema_name NOT IN ('information_schema', 'mysql', 'performance_schema', 'sys')
			  AND schema_name LIKE ?
			ORDER BY schema_name
		`
		queryArgs = append(queryArgs, pattern)
	} else {
		sqlStr = `
			SELECT schema_name, default_character_set_name, default_collation_name
			FROM information_schema.schemata
			WHERE schema_name NOT IN ('information_schema', 'mysql', 'performance_schema', 'sys')
			ORDER BY schema_name
		`
	}

	rows, err := m.db.QueryContext(queryCtx, sqlStr, queryArgs...)
	if err != nil {
		return "", fmt.Errorf("failed to query databases: %w", err)
	}
	defer rows.Close()

	type DatabaseMeta struct {
		Name      string `json:"name"`
		Charset   string `json:"charset,omitempty"`
		Collation string `json:"collation,omitempty"`
	}

	var dbs []DatabaseMeta
	for rows.Next() {
		var dbMeta DatabaseMeta
		if err := rows.Scan(&dbMeta.Name, &dbMeta.Charset, &dbMeta.Collation); err != nil {
			return "", err
		}
		dbs = append(dbs, dbMeta)
	}

	// 获取当前连接默认 schema (若 DSN 未包含则为空)
	var currentDB sql.NullString
	_ = m.db.QueryRowContext(queryCtx, "SELECT DATABASE()").Scan(&currentDB)

	res, err := json.MarshalIndent(map[string]any{
		"total_databases":  len(dbs),
		"databases":        dbs,
		"current_database": currentDB.String,
	}, "", "  ")
	if err != nil {
		return "", err
	}
	return string(res), nil
}

// HandleListTables 对应工具 mysql_list_tables
func (m *Module) HandleListTables(ctx context.Context, claims *casdoorsdk.Claims, args map[string]any) (string, error) {
	queryCtx, cancel := context.WithTimeout(ctx, time.Duration(m.cfg.QueryTimeoutMS)*time.Millisecond)
	defer cancel()

	schema, _ := args["schema"].(string)
	schema = strings.TrimSpace(strings.Trim(schema, "`"))

	var sqlStr string
	var queryArgs []any

	if schema != "" {
		sqlStr = `
			SELECT table_schema, table_name, table_type, IFNULL(engine, ''), IFNULL(table_rows, 0), IFNULL(table_comment, '')
			FROM information_schema.tables
			WHERE table_schema = ?
			ORDER BY table_name
		`
		queryArgs = append(queryArgs, schema)
	} else {
		// 未指定 schema 时：优先取连接默认库；若连接无默认库，则查询所有业务库
		sqlStr = `
			SELECT table_schema, table_name, table_type, IFNULL(engine, ''), IFNULL(table_rows, 0), IFNULL(table_comment, '')
			FROM information_schema.tables
			WHERE (
				(DATABASE() IS NOT NULL AND DATABASE() != '' AND table_schema = DATABASE())
				OR
				((DATABASE() IS NULL OR DATABASE() = '') AND table_schema NOT IN ('information_schema', 'mysql', 'performance_schema', 'sys'))
			)
			ORDER BY table_schema, table_name
		`
	}

	rows, err := m.db.QueryContext(queryCtx, sqlStr, queryArgs...)
	if err != nil {
		return "", fmt.Errorf("failed to query table schema: %w", err)
	}
	defer rows.Close()

	type TableMeta struct {
		Schema       string `json:"schema,omitempty"`
		Name         string `json:"name"`
		Type         string `json:"type"`
		Engine       string `json:"engine,omitempty"`
		RowsEstimate int64  `json:"rows_estimate"`
		Comment      string `json:"comment,omitempty"`
	}

	var tables []TableMeta
	for rows.Next() {
		var t TableMeta
		if err := rows.Scan(&t.Schema, &t.Name, &t.Type, &t.Engine, &t.RowsEstimate, &t.Comment); err != nil {
			return "", err
		}
		// 过滤黑名单表（支持 bare table 或 schema.table）
		if IsTableBlacklisted(t.Schema, t.Name, m.cfg.Security.TableBlacklist) {
			continue
		}
		tables = append(tables, t)
	}

	res, err := json.MarshalIndent(map[string]any{
		"total_tables": len(tables),
		"tables":       tables,
	}, "", "  ")
	if err != nil {
		return "", err
	}
	return string(res), nil
}

// HandleDescribeTable 对应工具 mysql_describe_table
func (m *Module) HandleDescribeTable(ctx context.Context, claims *casdoorsdk.Claims, args map[string]any) (string, error) {
	tableName, _ := args["table_name"].(string)
	tableName = strings.TrimSpace(tableName)
	if tableName == "" {
		return "", fmt.Errorf("missing required parameter 'table_name'")
	}

	targetSchema := ""
	targetTable := tableName

	// 支持 "schema.table" 传参格式
	if parts := strings.Split(tableName, "."); len(parts) == 2 {
		targetSchema = strings.TrimSpace(parts[0])
		targetTable = strings.TrimSpace(parts[1])
	} else if customSchema, _ := args["schema"].(string); strings.TrimSpace(customSchema) != "" {
		targetSchema = strings.TrimSpace(customSchema)
	}

	targetSchema = strings.Trim(targetSchema, "`")
	targetTable = strings.Trim(targetTable, "`")

	// 校验表是否属于黑名单（支持 bare table 与 schema.table）
	if IsTableBlacklisted(targetSchema, targetTable, m.cfg.Security.TableBlacklist) {
		return "", fmt.Errorf("access to table '%s' is prohibited by security policy", tableName)
	}

	queryCtx, cancel := context.WithTimeout(ctx, time.Duration(m.cfg.QueryTimeoutMS)*time.Millisecond)
	defer cancel()

	var sqlStr string
	var queryArgs []any

	if targetSchema != "" {
		sqlStr = `
			SELECT column_name, column_type, is_nullable, column_key, IFNULL(column_default, ''), extra, column_comment
			FROM information_schema.columns
			WHERE table_schema = ? AND table_name = ?
			ORDER BY ordinal_position
		`
		queryArgs = []any{targetSchema, targetTable}
	} else {
		sqlStr = `
			SELECT column_name, column_type, is_nullable, column_key, IFNULL(column_default, ''), extra, column_comment
			FROM information_schema.columns
			WHERE table_schema = DATABASE() AND table_name = ?
			ORDER BY ordinal_position
		`
		queryArgs = []any{targetTable}
	}

	rows, err := m.db.QueryContext(queryCtx, sqlStr, queryArgs...)
	if err != nil {
		return "", fmt.Errorf("failed to describe table '%s': %w", tableName, err)
	}
	defer rows.Close()

	type ColumnMeta struct {
		Field   string `json:"field"`
		Type    string `json:"type"`
		Null    string `json:"null"`
		Key     string `json:"key"`
		Default string `json:"default"`
		Extra   string `json:"extra"`
		Comment string `json:"comment"`
	}

	var columns []ColumnMeta
	for rows.Next() {
		var col ColumnMeta
		if err := rows.Scan(&col.Field, &col.Type, &col.Null, &col.Key, &col.Default, &col.Extra, &col.Comment); err != nil {
			return "", err
		}
		columns = append(columns, col)
	}

	if len(columns) == 0 {
		if targetSchema != "" {
			return "", fmt.Errorf("table '%s.%s' does not exist or has no columns", targetSchema, targetTable)
		}
		return "", fmt.Errorf("table '%s' does not exist in current database (or no default database selected, please specify 'schema')", targetTable)
	}

	output := map[string]any{
		"table":   targetTable,
		"columns": columns,
	}
	if targetSchema != "" {
		output["schema"] = targetSchema
	}

	res, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return "", err
	}
	return string(res), nil
}

// HandleQuery 对应工具 mysql_query
func (m *Module) HandleQuery(ctx context.Context, claims *casdoorsdk.Claims, args map[string]any) (string, error) {
	query, _ := args["query"].(string)
	if strings.TrimSpace(query) == "" {
		return "", fmt.Errorf("missing required parameter 'query'")
	}

	customSchema, _ := args["schema"].(string)
	customSchema = strings.TrimSpace(customSchema)

	maxLimit := m.cfg.Security.MaxLimit
	if customLimit, ok := args["max_rows"]; ok {
		if limitInt, ok := toInt(customLimit); ok && limitInt > 0 && limitInt < maxLimit {
			maxLimit = limitInt
		}
	}

	// 1. AST 语法强校验、可选 schema 补充与强制 LIMIT 改写
	safeSQL, err := ValidateAndRewriteSQLWithSchema(query, customSchema, maxLimit, m.cfg.Security.TableBlacklist)
	if err != nil {
		return "", fmt.Errorf("security check failed: %w", err)
	}

	queryCtx, cancel := context.WithTimeout(ctx, time.Duration(m.cfg.QueryTimeoutMS)*time.Millisecond)
	defer cancel()

	start := time.Now()
	rows, err := m.db.QueryContext(queryCtx, safeSQL)
	if err != nil {
		return "", fmt.Errorf("SQL execution failed: %w", err)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return "", err
	}

	var results []map[string]any
	truncated := false
	for rows.Next() {
		values := make([]any, len(columns))
		valuePtrs := make([]any, len(columns))
		for i := range values {
			valuePtrs[i] = &values[i]
		}

		if err := rows.Scan(valuePtrs...); err != nil {
			return "", err
		}

		rowMap := make(map[string]any, len(columns))
		for i, col := range columns {
			val := values[i]
			if b, ok := val.([]byte); ok {
				rowMap[col] = truncateCell(string(b))
			} else if s, ok := val.(string); ok {
				rowMap[col] = truncateCell(s)
			} else {
				rowMap[col] = val
			}
		}
		results = append(results, rowMap)

		// 行数上限之外再守一道累计字节上限：宽行或大字段会在行数耗尽前就撑满上下文。
		if estimateRowsBytes(results) > maxResultBytes {
			truncated = true
			break
		}
	}

	elapsed := time.Since(start)

	output := map[string]any{
		"query_executed":    safeSQL,
		"rows_returned":     len(results),
		"truncated":         truncated,
		"execution_time_ms": elapsed.Milliseconds(),
		"rows":              results,
	}

	res, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return "", err
	}
	return string(res), nil
}

const (
	// maxResultBytes 是 mysql_query 返回给 LLM 的累计上限，与 Redis 侧保持一致。
	maxResultBytes = 64 * 1024
	// maxCellBytes 是单个单元格文本的上限，防止一行 TEXT/JSON 就占满全部额度。
	maxCellBytes = 4 * 1024
)

// truncateCell 把超长单元格缩略，保留前缀并标注被截断的原始长度。
func truncateCell(s string) string {
	if len(s) <= maxCellBytes {
		return s
	}
	return s[:maxCellBytes] + fmt.Sprintf("... [truncated, %d bytes total]", len(s))
}

// estimateRowsBytes 估算已收集行的文本体积，用于在序列化前提前停止扫描。
func estimateRowsBytes(rows []map[string]any) int {
	total := 0
	for _, row := range rows {
		for k, v := range row {
			total += len(k)
			if s, ok := v.(string); ok {
				total += len(s)
			}
		}
	}
	return total
}

func toInt(val any) (int, bool) {
	switch v := val.(type) {
	case int:
		return v, true
	case float64:
		return int(v), true
	case int64:
		return int(v), true
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n, true
		}
		return 0, false
	default:
		return 0, false
	}
}

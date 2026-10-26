package mysql

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/casdoor/casdoor-go-sdk/casdoorsdk"
)

// HandleListTables 对应工具 mysql_list_tables
func (m *Module) HandleListTables(ctx context.Context, claims *casdoorsdk.Claims, args map[string]any) (string, error) {
	queryCtx, cancel := context.WithTimeout(ctx, time.Duration(m.cfg.QueryTimeoutMS)*time.Millisecond)
	defer cancel()

	sqlStr := `
		SELECT table_name, table_type, IFNULL(engine, ''), IFNULL(table_rows, 0), IFNULL(table_comment, '')
		FROM information_schema.tables
		WHERE table_schema = DATABASE()
		ORDER BY table_name
	`

	rows, err := m.db.QueryContext(queryCtx, sqlStr)
	if err != nil {
		return "", fmt.Errorf("failed to query table schema: %w", err)
	}
	defer rows.Close()

	type TableMeta struct {
		Name         string `json:"name"`
		Type         string `json:"type"`
		Engine       string `json:"engine,omitempty"`
		RowsEstimate int64  `json:"rows_estimate"`
		Comment      string `json:"comment,omitempty"`
	}

	var tables []TableMeta
	for rows.Next() {
		var t TableMeta
		if err := rows.Scan(&t.Name, &t.Type, &t.Engine, &t.RowsEstimate, &t.Comment); err != nil {
			return "", err
		}
		// 过滤黑名单表（支持 bare table 或 schema.table）
		if IsTableBlacklisted("", t.Name, m.cfg.Security.TableBlacklist) {
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

	// 校验表是否属于黑名单（支持 bare table 与 schema.table）
	schema := ""
	table := tableName
	if parts := strings.Split(tableName, "."); len(parts) == 2 {
		schema = parts[0]
		table = parts[1]
	}
	if IsTableBlacklisted(schema, table, m.cfg.Security.TableBlacklist) {
		return "", fmt.Errorf("access to table '%s' is prohibited by security policy", tableName)
	}

	queryCtx, cancel := context.WithTimeout(ctx, time.Duration(m.cfg.QueryTimeoutMS)*time.Millisecond)
	defer cancel()

	sqlStr := `
		SELECT column_name, column_type, is_nullable, column_key, IFNULL(column_default, ''), extra, column_comment
		FROM information_schema.columns
		WHERE table_schema = DATABASE() AND table_name = ?
		ORDER BY ordinal_position
	`

	rows, err := m.db.QueryContext(queryCtx, sqlStr, tableName)
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
		return "", fmt.Errorf("table '%s' does not exist or has no columns", tableName)
	}

	res, err := json.MarshalIndent(map[string]any{
		"table":   tableName,
		"columns": columns,
	}, "", "  ")
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

	maxLimit := m.cfg.Security.MaxLimit
	if customLimit, ok := args["max_rows"]; ok {
		if limitInt, ok := toInt(customLimit); ok && limitInt > 0 && limitInt < maxLimit {
			maxLimit = limitInt
		}
	}

	// 1. AST 语法强校验与强制 LIMIT 改写
	safeSQL, err := ValidateAndRewriteSQL(query, maxLimit, m.cfg.Security.TableBlacklist)
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

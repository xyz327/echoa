package mysql

import (
	"fmt"
	"math"
	"strings"

	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/format"
	_ "github.com/pingcap/tidb/pkg/parser/test_driver"
)

type tableRef struct {
	schema string
	name   string
}

type tableVisitor struct {
	tables []tableRef
}

func (v *tableVisitor) Enter(in ast.Node) (ast.Node, bool) {
	if t, ok := in.(*ast.TableName); ok {
		v.tables = append(v.tables, tableRef{
			schema: t.Schema.L,
			name:   t.Name.L,
		})
	}
	return in, false
}

func (v *tableVisitor) Leave(in ast.Node) (ast.Node, bool) {
	return in, true
}

type safetyVisitor struct {
	hasInto bool
	hasLock bool
}

func (v *safetyVisitor) Enter(in ast.Node) (ast.Node, bool) {
	if s, ok := in.(*ast.SelectStmt); ok {
		if s.SelectIntoOpt != nil {
			v.hasInto = true
		}
		if s.LockInfo != nil && s.LockInfo.LockType != ast.SelectLockNone {
			v.hasLock = true
		}
	}
	return in, false
}

func (v *safetyVisitor) Leave(in ast.Node) (ast.Node, bool) {
	return in, true
}

type schemaQualifierVisitor struct {
	defaultSchema string
}

func (v *schemaQualifierVisitor) Enter(in ast.Node) (ast.Node, bool) {
	if v.defaultSchema != "" {
		if t, ok := in.(*ast.TableName); ok {
			if t.Schema.L == "" && !t.IsAlias {
				t.Schema = ast.NewCIStr(v.defaultSchema)
			}
		}
	}
	return in, false
}

func (v *schemaQualifierVisitor) Leave(in ast.Node) (ast.Node, bool) {
	return in, true
}

// IsTableBlacklisted 判断指定表（及可选库名）是否命中黑名单
// 支持 "table"（匹配任意库下的该表）与 "schema.table"（精准匹配特定库下的该表），支持自动去除反引号
func IsTableBlacklisted(schema, table string, blacklist []string) bool {
	if len(blacklist) == 0 {
		return false
	}
	table = strings.ToLower(strings.Trim(table, "`"))
	schema = strings.ToLower(strings.Trim(schema, "`"))
	qualified := ""
	if schema != "" {
		qualified = schema + "." + table
	}

	for _, item := range blacklist {
		clean := strings.ToLower(strings.TrimSpace(item))
		clean = strings.ReplaceAll(clean, "`", "")
		if clean == "" {
			continue
		}
		if strings.Contains(clean, ".") {
			if qualified != "" && clean == qualified {
				return true
			}
		} else {
			if clean == table {
				return true
			}
		}
	}
	return false
}

// DefaultMaxQueryOffset 是未显式配置 security.max_offset 时的 OFFSET 硬上限。
// OFFSET 不限制返回行数，却要求服务端先把前面所有行扫过并丢弃，是 LIMIT 改写之外
// 另一个能把小查询放大成全表扫描的口子。
const DefaultMaxQueryOffset = 100000

// ValidateAndRewriteSQL 校验 SQL 语句并注入/改写 LIMIT（保持无默认 schema 兼容）
func ValidateAndRewriteSQL(sqlStr string, maxLimit int, blacklist []string) (string, error) {
	return ValidateAndRewriteSQLWithSchema(sqlStr, "", maxLimit, DefaultMaxQueryOffset, blacklist)
}

// ValidateAndRewriteSQLWithSchema 校验 SQL 语句，若传入 defaultSchema，自动为无前缀的数据表补充 schema 限定
func ValidateAndRewriteSQLWithSchema(sqlStr string, defaultSchema string, maxLimit, maxOffset int, blacklist []string) (string, error) {
	trimmed := strings.TrimSpace(sqlStr)
	// 去除末尾分号
	trimmed = strings.TrimRight(trimmed, ";")
	trimmed = strings.TrimSpace(trimmed)
	defaultSchema = strings.TrimSpace(strings.Trim(defaultSchema, "`"))

	p := parser.New()
	stmts, _, err := p.Parse(trimmed, "", "")
	if err != nil {
		return "", fmt.Errorf("SQL syntax error: %w", err)
	}

	if len(stmts) == 0 {
		return "", fmt.Errorf("empty SQL statement")
	}
	if len(stmts) > 1 {
		return "", fmt.Errorf("multiple SQL statements are strictly forbidden")
	}

	stmt := stmts[0]
	isQuery := false

	// 深度只读安全审查：遍历 AST 树拦截任意层级的 SELECT INTO 写盘与加锁读取 (FOR UPDATE / LOCK IN SHARE MODE)
	sv := &safetyVisitor{}
	stmt.Accept(sv)
	if sv.hasInto {
		return "", fmt.Errorf("SELECT INTO OUTFILE/DUMPFILE is strictly forbidden")
	}
	if sv.hasLock {
		return "", fmt.Errorf("locking read statements (FOR UPDATE, LOCK IN SHARE MODE) are strictly forbidden in read-only mode")
	}

	switch s := stmt.(type) {
	case *ast.SelectStmt:
		isQuery = true
		if err := rewriteLimit(s.Limit, maxLimit, maxOffset, func(l *ast.Limit) { s.Limit = l }); err != nil {
			return "", err
		}
	case *ast.SetOprStmt:
		isQuery = true
		if err := rewriteLimit(s.Limit, maxLimit, maxOffset, func(l *ast.Limit) { s.Limit = l }); err != nil {
			return "", err
		}
	case *ast.ExplainStmt:
		// EXPLAIN 的语法同样接受 INSERT/UPDATE/DELETE，只读工具只放行 SELECT / UNION。
		// 普通 EXPLAIN 只读执行计划可以原样通过；但 EXPLAIN ANALYZE（MySQL 8.0.18+ / MariaDB）
		// 会真正执行内层语句，不注入 LIMIT 等于把 max_limit 整个绕开，因此按普通查询同等对待。
		switch inner := s.Stmt.(type) {
		case *ast.SelectStmt:
			if s.Analyze {
				isQuery = true
				if err := rewriteLimit(inner.Limit, maxLimit, maxOffset, func(l *ast.Limit) { inner.Limit = l }); err != nil {
					return "", err
				}
			}
		case *ast.SetOprStmt:
			if s.Analyze {
				isQuery = true
				if err := rewriteLimit(inner.Limit, maxLimit, maxOffset, func(l *ast.Limit) { inner.Limit = l }); err != nil {
					return "", err
				}
			}
		default:
			return "", fmt.Errorf("EXPLAIN is only allowed on SELECT/UNION statements in read-only mode, got: %T", s.Stmt)
		}
	case *ast.ShowStmt:
		// SHOW 允许通过；若指定了 defaultSchema 且为 SHOW TABLES，为其补充 FROM <defaultSchema>
		if defaultSchema != "" && s.Tp == ast.ShowTables && s.DBName == "" {
			s.DBName = defaultSchema
		}
	default:
		return "", fmt.Errorf("only read-only statements (SELECT, EXPLAIN, SHOW) are allowed, got: %T", s)
	}

	// 1. 若指定了 defaultSchema，自动为无 schema 前缀的数据表补充限定
	if defaultSchema != "" {
		qv := &schemaQualifierVisitor{defaultSchema: defaultSchema}
		stmt.Accept(qv)
	}

	// 2. 检查表黑名单 (支持 'table' 与 'schema.table')
	if len(blacklist) > 0 {
		v := &tableVisitor{}
		stmt.Accept(v)

		for _, ref := range v.tables {
			if IsTableBlacklisted(ref.schema, ref.name, blacklist) {
				tblName := ref.name
				if ref.schema != "" {
					tblName = ref.schema + "." + ref.name
				}
				return "", fmt.Errorf("access to table '%s' is prohibited by security policy", tblName)
			}
		}
	}

	// 3. 对于 SELECT / UNION / SHOW 类或发生改写的查询，通过 AST Restore 还原注入/改写后的安全 SQL
	if isQuery || defaultSchema != "" {
		var sb strings.Builder
		restoreCtx := format.NewRestoreCtx(format.DefaultRestoreFlags, &sb)
		if err := stmt.Restore(restoreCtx); err != nil {
			return "", fmt.Errorf("failed to restore sanitized SQL: %w", err)
		}
		return sb.String(), nil
	}

	return trimmed, nil
}

func rewriteLimit(currentLimit *ast.Limit, maxLimit, maxOffset int, setLimit func(*ast.Limit)) error {
	if maxLimit <= 0 {
		maxLimit = 200
	}
	if err := checkLimitOffset(currentLimit, maxOffset); err != nil {
		return err
	}

	if currentLimit == nil {
		setLimit(&ast.Limit{
			Count: ast.NewValueExpr(maxLimit, "", ""),
		})
		return nil
	}

	needRewrite := false
	if valExpr, ok := currentLimit.Count.(ast.ValueExpr); ok {
		var countVal int64
		switch v := valExpr.GetValue().(type) {
		case int64:
			countVal = v
		case uint64:
			countVal = int64(v)
		case int:
			countVal = int64(v)
		default:
			needRewrite = true
		}
		if !needRewrite && countVal > int64(maxLimit) {
			needRewrite = true
		}
	} else {
		needRewrite = true
	}

	if needRewrite {
		currentLimit.Count = ast.NewValueExpr(maxLimit, "", "")
	}
	return nil
}

// checkLimitOffset 拒绝超过上限的 OFFSET。返回行数已经受 max_limit 约束，但 OFFSET 越大，
// 服务端要先扫过并丢弃的行数就越多（LIMIT 100000000, 10 依然是一次全表扫描），
// 因此 offset 必须有独立上限；这里选择拒绝而不是改写，改写会静默改变查询语义。
func checkLimitOffset(currentLimit *ast.Limit, maxOffset int) error {
	if currentLimit == nil || currentLimit.Offset == nil {
		return nil
	}
	if maxOffset <= 0 {
		maxOffset = DefaultMaxQueryOffset
	}

	valExpr, ok := currentLimit.Offset.(ast.ValueExpr)
	if !ok {
		return fmt.Errorf("LIMIT offset must be a literal integer to be verified against max_offset")
	}

	var offset int64
	switch v := valExpr.GetValue().(type) {
	case int64:
		offset = v
	case uint64:
		if v > math.MaxInt64 {
			return fmt.Errorf("LIMIT offset %d exceeds the maximum allowed offset (%d)", uint64(v), maxOffset)
		}
		offset = int64(v)
	case int:
		offset = int64(v)
	default:
		return fmt.Errorf("LIMIT offset must be a literal integer to be verified against max_offset")
	}

	if offset > int64(maxOffset) {
		return fmt.Errorf("LIMIT offset %d exceeds the maximum allowed offset (%d), narrow the query instead of deep paging", offset, maxOffset)
	}
	return nil
}

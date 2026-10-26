package mysql

import (
	"fmt"
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

// ValidateAndRewriteSQL 校验 SQL 语句并注入/改写 LIMIT
func ValidateAndRewriteSQL(sqlStr string, maxLimit int, blacklist []string) (string, error) {
	trimmed := strings.TrimSpace(sqlStr)
	// 去除末尾分号
	trimmed = strings.TrimRight(trimmed, ";")
	trimmed = strings.TrimSpace(trimmed)

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
		rewriteLimit(s.Limit, maxLimit, func(l *ast.Limit) { s.Limit = l })
	case *ast.SetOprStmt:
		isQuery = true
		rewriteLimit(s.Limit, maxLimit, func(l *ast.Limit) { s.Limit = l })
	case *ast.ExplainStmt:
		// EXPLAIN 允许通过
	case *ast.ShowStmt:
		// SHOW 允许通过
	default:
		return "", fmt.Errorf("only read-only statements (SELECT, EXPLAIN, SHOW) are allowed, got: %T", s)
	}

	// 1. 检查表黑名单 (支持 'table' 与 'schema.table')
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

	// 2. 对于 SELECT / UNION 类查询，通过 AST Restore 还原注入/改写后的安全 SQL
	if isQuery {
		var sb strings.Builder
		restoreCtx := format.NewRestoreCtx(format.DefaultRestoreFlags, &sb)
		if err := stmt.Restore(restoreCtx); err != nil {
			return "", fmt.Errorf("failed to restore sanitized SQL: %w", err)
		}
		return sb.String(), nil
	}

	return trimmed, nil
}

func rewriteLimit(currentLimit *ast.Limit, maxLimit int, setLimit func(*ast.Limit)) {
	if maxLimit <= 0 {
		maxLimit = 200
	}

	if currentLimit == nil {
		setLimit(&ast.Limit{
			Count: ast.NewValueExpr(maxLimit, "", ""),
		})
		return
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
}

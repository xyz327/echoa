package mysql

import (
	"strings"
	"testing"
)

func TestValidateAndRewriteSQL(t *testing.T) {
	blacklist := []string{"user_passwords", "secret_tokens", "sensitive_db.payroll", "`vault`.`keys`"}

	// 1. SELECT without LIMIT -> 注入 LIMIT 100
	sql1 := "SELECT id, name FROM users WHERE active = 1"
	rewritten1, err := ValidateAndRewriteSQL(sql1, 100, blacklist)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasSuffix(rewritten1, "LIMIT 100") {
		t.Errorf("expected LIMIT 100 at end, got: %s", rewritten1)
	}

	// 2. SELECT with excessive LIMIT -> 改写为 LIMIT 100
	sql2 := "SELECT id FROM users LIMIT 500"
	rewritten2, err := ValidateAndRewriteSQL(sql2, 100, blacklist)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasSuffix(rewritten2, "LIMIT 100") {
		t.Errorf("expected LIMIT 100 at end, got: %s", rewritten2)
	}

	// 3. SELECT with acceptable LIMIT -> 保持 LIMIT 50
	sql3 := "SELECT id FROM users LIMIT 50"
	rewritten3, err := ValidateAndRewriteSQL(sql3, 100, blacklist)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasSuffix(rewritten3, "LIMIT 50") {
		t.Errorf("expected LIMIT 50 at end, got: %s", rewritten3)
	}

	// 4. LIMIT offset, count 且 count 超标 -> 保持 offset，仅改写 count
	sql4 := "SELECT id FROM users LIMIT 20, 500"
	rewritten4, err := ValidateAndRewriteSQL(sql4, 100, blacklist)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasSuffix(rewritten4, "LIMIT 20,100") && !strings.HasSuffix(rewritten4, "LIMIT 20, 100") {
		t.Errorf("expected offset 20 and count 100, got: %s", rewritten4)
	}

	// 5. 子查询中已有 LIMIT，外层查询无 LIMIT -> 必须为外层追加 LIMIT
	sql5 := "SELECT * FROM (SELECT id, name FROM users LIMIT 10) AS sub"
	rewritten5, err := ValidateAndRewriteSQL(sql5, 100, blacklist)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasSuffix(rewritten5, "LIMIT 100") {
		t.Errorf("outer query should have LIMIT 100, got: %s", rewritten5)
	}

	// 6. SQL 包含含 LIMIT 关键字的字符串常量 -> 字符串不被篡改，外层追加 LIMIT
	sql6 := "SELECT id FROM users WHERE note = 'Please do not LIMIT 9999' AND status = 1"
	rewritten6, err := ValidateAndRewriteSQL(sql6, 100, blacklist)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(rewritten6, "'Please do not LIMIT 9999'") {
		t.Errorf("string literal should remain intact, got: %s", rewritten6)
	}
	if !strings.HasSuffix(rewritten6, "LIMIT 100") {
		t.Errorf("expected LIMIT 100 at end, got: %s", rewritten6)
	}

	// 7. 注释中包含 LIMIT -> 不被干扰，外层追加 LIMIT
	sql7 := "SELECT id FROM users /* LIMIT 10 */ WHERE active = 1"
	rewritten7, err := ValidateAndRewriteSQL(sql7, 100, blacklist)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasSuffix(rewritten7, "LIMIT 100") {
		t.Errorf("expected LIMIT 100 at end, got: %s", rewritten7)
	}

	// 8. UNION 查询 -> 外层追加 LIMIT
	sql8 := "SELECT id FROM users WHERE id = 1 UNION SELECT id FROM users WHERE id = 2"
	rewritten8, err := ValidateAndRewriteSQL(sql8, 100, blacklist)
	if err != nil {
		t.Fatalf("unexpected error for UNION query: %v", err)
	}
	if !strings.HasSuffix(rewritten8, "LIMIT 100") {
		t.Errorf("UNION query should have LIMIT 100 at end, got: %s", rewritten8)
	}

	// 9. 命中表黑名单 (单表)
	sql9 := "SELECT * FROM secret_tokens"
	_, err = ValidateAndRewriteSQL(sql9, 100, blacklist)
	if err == nil {
		t.Fatalf("expected error for blacklisted table 'secret_tokens', got nil")
	}

	// 10. 命中表黑名单 (跨库精准匹配 sensitive_db.payroll)
	sql10 := "SELECT * FROM sensitive_db.payroll"
	_, err = ValidateAndRewriteSQL(sql10, 100, blacklist)
	if err == nil {
		t.Fatalf("expected error for blacklisted table 'sensitive_db.payroll', got nil")
	}

	// 11. 未命中 schema 黑名单 (other_db.payroll 不在名单中)
	sql11 := "SELECT * FROM other_db.payroll"
	_, err = ValidateAndRewriteSQL(sql11, 100, blacklist)
	if err != nil {
		t.Fatalf("unexpected error for non-blacklisted other_db.payroll: %v", err)
	}

	// 12. 命中反引号包裹的黑名单 vault.keys
	sql12 := "SELECT * FROM `vault`.`keys`"
	_, err = ValidateAndRewriteSQL(sql12, 100, blacklist)
	if err == nil {
		t.Fatalf("expected error for blacklisted table '`vault`.`keys`', got nil")
	}

	// 13. JOIN 子句中命中黑名单
	sql13 := "SELECT u.id, s.token FROM users u JOIN secret_tokens s ON u.id = s.user_id"
	_, err = ValidateAndRewriteSQL(sql13, 100, blacklist)
	if err == nil {
		t.Fatalf("expected error for blacklisted table in JOIN, got nil")
	}

	// 14. 写语句严格拦截
	sql14 := "DELETE FROM users WHERE id = 1"
	_, err = ValidateAndRewriteSQL(sql14, 100, blacklist)
	if err == nil {
		t.Fatalf("expected error for DELETE statement, got nil")
	}

	// 15. 多语句严格拦截
	sql15 := "SELECT 1; DROP TABLE users"
	_, err = ValidateAndRewriteSQL(sql15, 100, blacklist)
	if err == nil {
		t.Fatalf("expected error for multi-statement, got nil")
	}

	// 16. EXPLAIN 与 SHOW 允许通过且不强制注入 LIMIT
	sql16 := "EXPLAIN SELECT * FROM users"
	rewritten16, err := ValidateAndRewriteSQL(sql16, 100, blacklist)
	if err != nil {
		t.Fatalf("unexpected error for EXPLAIN: %v", err)
	}
	if rewritten16 != sql16 {
		t.Errorf("EXPLAIN should be returned unmodified, got: %s", rewritten16)
	}

	sql17 := "SHOW TABLES"
	rewritten17, err := ValidateAndRewriteSQL(sql17, 100, blacklist)
	if err != nil {
		t.Fatalf("unexpected error for SHOW: %v", err)
	}
	if rewritten17 != sql17 {
		t.Errorf("SHOW should be returned unmodified, got: %s", rewritten17)
	}

	// 18. SELECT ... INTO OUTFILE 写盘语句必须拦截
	sql18 := "SELECT id, name FROM users INTO OUTFILE '/tmp/users.csv'"
	_, err = ValidateAndRewriteSQL(sql18, 100, blacklist)
	if err == nil {
		t.Fatalf("expected error for SELECT INTO OUTFILE, got nil")
	}
	if !strings.Contains(err.Error(), "INTO OUTFILE/DUMPFILE") {
		t.Errorf("expected INTO OUTFILE rejection, got: %v", err)
	}

	// 19. SELECT ... INTO DUMPFILE 同样拦截
	sql19 := "SELECT password FROM users INTO DUMPFILE '/tmp/dump.bin'"
	_, err = ValidateAndRewriteSQL(sql19, 100, blacklist)
	if err == nil {
		t.Fatalf("expected error for SELECT INTO DUMPFILE, got nil")
	}

	// 20. SELECT ... FOR UPDATE 锁表排他锁必须拦截
	sql20 := "SELECT id, name FROM users WHERE id = 1 FOR UPDATE"
	_, err = ValidateAndRewriteSQL(sql20, 100, blacklist)
	if err == nil {
		t.Fatalf("expected error for SELECT ... FOR UPDATE, got nil")
	}
	if !strings.Contains(err.Error(), "locking read statements") {
		t.Errorf("expected locking read error, got: %v", err)
	}

	// 21. SELECT ... LOCK IN SHARE MODE 共享锁必须拦截
	sql21 := "SELECT id FROM users LOCK IN SHARE MODE"
	_, err = ValidateAndRewriteSQL(sql21, 100, blacklist)
	if err == nil {
		t.Fatalf("expected error for SELECT ... LOCK IN SHARE MODE, got nil")
	}

	// 22. SELECT ... FOR SHARE 与 NOWAIT 同样拦截
	sql22 := "SELECT id FROM users FOR SHARE NOWAIT"
	_, err = ValidateAndRewriteSQL(sql22, 100, blacklist)
	if err == nil {
		t.Fatalf("expected error for SELECT ... FOR SHARE NOWAIT, got nil")
	}

	// 23. UNION 语句中分支带有 FOR UPDATE 必须拦截
	sql23 := "SELECT id FROM users WHERE id = 1 UNION SELECT id FROM users WHERE id = 2 FOR UPDATE"
	_, err = ValidateAndRewriteSQL(sql23, 100, blacklist)
	if err == nil {
		t.Fatalf("expected error for UNION containing FOR UPDATE, got nil")
	}

	// 24. 派生子查询中带有 FOR UPDATE 必须拦截
	sql24 := "SELECT * FROM (SELECT id FROM users FOR UPDATE) AS t"
	_, err = ValidateAndRewriteSQL(sql24, 100, blacklist)
	if err == nil {
		t.Fatalf("expected error for subquery containing FOR UPDATE, got nil")
	}
}

func TestValidateAndRewriteSQLWithSchema(t *testing.T) {
	blacklist := []string{"vault.secret_tokens", "users_secret"}

	// 1. 指定 defaultSchema，无前缀表名自动补充 schema 限定
	sql1 := "SELECT id, name FROM users WHERE active = 1"
	rewritten1, err := ValidateAndRewriteSQLWithSchema(sql1, "billing", 100, blacklist)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(rewritten1, "`billing`.`users`") {
		t.Errorf("expected table qualified with `billing`.`users`, got: %s", rewritten1)
	}
	if !strings.HasSuffix(rewritten1, "LIMIT 100") {
		t.Errorf("expected LIMIT 100 at end, got: %s", rewritten1)
	}

	// 2. 表名已有明确 schema 限定时，不被 defaultSchema 覆盖
	sql2 := "SELECT id, name FROM orders.users WHERE active = 1"
	rewritten2, err := ValidateAndRewriteSQLWithSchema(sql2, "billing", 100, blacklist)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(rewritten2, "`orders`.`users`") {
		t.Errorf("expected original schema `orders`.`users` preserved, got: %s", rewritten2)
	}

	// 3. 带别名的表，别名不应被误加 schema 限定
	sql3 := "SELECT u.id, u.name FROM users AS u WHERE u.id = 1"
	rewritten3, err := ValidateAndRewriteSQLWithSchema(sql3, "crm", 100, blacklist)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(rewritten3, "`crm`.`users`") {
		t.Errorf("table should be qualified with `crm`.`users`, got: %s", rewritten3)
	}
	if strings.Contains(rewritten3, "`crm`.`u`") {
		t.Errorf("alias 'u' must not be qualified with schema, got: %s", rewritten3)
	}

	// 4. SHOW TABLES 传入 defaultSchema 自动改写为 SHOW TABLES FROM/IN <defaultSchema>
	sql4 := "SHOW TABLES"
	rewritten4, err := ValidateAndRewriteSQLWithSchema(sql4, "analytics", 100, blacklist)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(rewritten4, "SHOW TABLES IN `analytics`") && !strings.Contains(rewritten4, "SHOW TABLES FROM `analytics`") {
		t.Errorf("expected SHOW TABLES IN/FROM `analytics`, got: %s", rewritten4)
	}

	// 5. 命中 schema.table 组合黑名单
	sql5 := "SELECT * FROM secret_tokens"
	_, err = ValidateAndRewriteSQLWithSchema(sql5, "vault", 100, blacklist)
	if err == nil {
		t.Fatalf("expected error for blacklisted vault.secret_tokens, got nil")
	}
}

package redis

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"echoa/internal/config"
)

func TestRedisSecurityPolicy(t *testing.T) {
	cfg := config.RedisConfig{
		Enabled:    true,
		Addr:       "127.0.0.1:6379",
		AllowedDBs: []int{0, 1, 2},
		Security: struct {
			AllowedCommands []string `mapstructure:"allowed_commands"`
			BlockedCommands []string `mapstructure:"blocked_commands"`
		}{
			AllowedCommands: []string{"GET", "HGETALL", "SCAN", "LRANGE"},
			BlockedCommands: []string{"MONITOR"},
		},
	}

	allowedMap := make(map[string]bool)
	for _, cmd := range cfg.Security.AllowedCommands {
		allowedMap[strings.ToUpper(cmd)] = true
	}
	blockedMap := make(map[string]bool)
	for _, cmd := range cfg.Security.BlockedCommands {
		blockedMap[strings.ToUpper(cmd)] = true
	}

	m := &Module{
		cfg:        cfg,
		dbMgr:      NewMultiDBManager(&redis.Options{Addr: cfg.Addr}, cfg.AllowedDBs),
		allowedMap: allowedMap,
		blockedMap: blockedMap,
	}

	ctx := context.Background()

	// 1. 测试 KEYS 拦截
	_, err := m.HandleExecute(ctx, nil, map[string]any{"command": "KEYS", "args": []any{"*"}})
	if err == nil || !strings.Contains(err.Error(), "blocked for safety") {
		t.Fatalf("expected KEYS command to be blocked, got err: %v", err)
	}

	// 2. 测试 SELECT 拦截
	_, err = m.HandleExecute(ctx, nil, map[string]any{"command": "SELECT", "args": []any{"1"}})
	if err == nil || !strings.Contains(err.Error(), "manual 'SELECT' command is forbidden") {
		t.Fatalf("expected SELECT command to be blocked, got err: %v", err)
	}

	// 3. 测试硬黑名单写入拦截 (SET)
	_, err = m.HandleExecute(ctx, nil, map[string]any{"command": "SET", "args": []any{"k", "v"}})
	if err == nil || !strings.Contains(err.Error(), "strictly forbidden") {
		t.Fatalf("expected SET command to be blocked, got err: %v", err)
	}

	// 4. 测试硬黑名单清库拦截 (FLUSHALL)
	_, err = m.HandleExecute(ctx, nil, map[string]any{"command": "FLUSHALL", "args": []any{}})
	if err == nil || !strings.Contains(err.Error(), "strictly forbidden") {
		t.Fatalf("expected FLUSHALL command to be blocked, got err: %v", err)
	}

	// 5. 测试不在白名单内的只读命令 (INFO)
	_, err = m.HandleExecute(ctx, nil, map[string]any{"command": "INFO", "args": []any{}})
	if err == nil || !strings.Contains(err.Error(), "not in the allowed read-only whitelist") {
		t.Fatalf("expected INFO command to be not in whitelist, got err: %v", err)
	}

	// 6. 测试未授权的 DB 访问 (如 DB 9)
	_, err = m.HandleExecute(ctx, nil, map[string]any{"command": "GET", "args": []any{"key1"}, "db": 9})
	if err == nil || !strings.Contains(err.Error(), "not permitted by configuration") {
		t.Fatalf("expected DB 9 to be rejected, got err: %v", err)
	}
}

func TestFormatAndTruncateResult(t *testing.T) {
	// 1. []string 截断测试
	strSlice := make([]string, 100)
	for i := 0; i < 100; i++ {
		strSlice[i] = fmt.Sprintf("item-%d", i)
	}
	out, err := formatAndTruncateResult(strSlice, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "... [Output truncated: collections capped to first 10 items/keys]") {
		t.Errorf("expected slice truncation banner, got: %s", out)
	}

	// 2. []any 截断测试
	anySlice := make([]any, 50)
	for i := 0; i < 50; i++ {
		anySlice[i] = i
	}
	out, err = formatAndTruncateResult(anySlice, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "... [Output truncated: collections capped to first 5 items/keys]") {
		t.Errorf("expected slice truncation banner, got: %s", out)
	}

	// 3. map[string]string 截断测试
	mapStr := make(map[string]string, 50)
	for i := 0; i < 50; i++ {
		mapStr[fmt.Sprintf("k%d", i)] = fmt.Sprintf("v%d", i)
	}
	out, err = formatAndTruncateResult(mapStr, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "... [Output truncated: collections capped to first 5 items/keys]") {
		t.Errorf("expected map truncation banner, got: %s", out)
	}

	// 4. map[any]any (RESP3 协议常见返回类型)
	mapAny := make(map[any]any, 30)
	for i := 0; i < 30; i++ {
		mapAny[fmt.Sprintf("resp3_key_%d", i)] = i
	}
	out, err = formatAndTruncateResult(mapAny, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "... [Output truncated: collections capped to first 5 items/keys]") {
		t.Errorf("expected map[any]any truncation banner, got: %s", out)
	}

	// 5. 极端超大字符串 (触发 maxOutputBytes 64KB 熔断)
	hugeString := strings.Repeat("a", 100*1024)
	out, err = formatAndTruncateResult(hugeString, 200)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "... [Output truncated: total response size capped to 65536 bytes]") &&
		!strings.Contains(out, "... [string value truncated]") {
		t.Errorf("expected byte limit truncation banner, got: %s", out)
	}
	if len(out) > 70*1024 {
		t.Errorf("expected output to be constrained near 64KB, got len: %d", len(out))
	}

	// 6. toInt 增强测试
	if n, ok := toInt("3"); !ok || n != 3 {
		t.Errorf("expected toInt(\"3\") to be 3, got: %v, %v", n, ok)
	}
	if n, ok := toInt(7); !ok || n != 7 {
		t.Errorf("expected toInt(7) to be 7, got: %v, %v", n, ok)
	}
	if n, ok := toInt(float64(9)); !ok || n != 9 {
		t.Errorf("expected toInt(9.0) to be 9, got: %v, %v", n, ok)
	}
	if _, ok := toInt("invalid"); ok {
		t.Errorf("expected toInt(\"invalid\") to fail")
	}
}

func TestMultiDBManagerPoolOptions(t *testing.T) {
	baseOpts := &redis.Options{
		Addr:            "127.0.0.1:6379",
		PoolSize:        25,
		MinIdleConns:    8,
		ConnMaxIdleTime: 10 * time.Minute,
		ConnMaxLifetime: 1 * time.Hour,
	}
	mgr := NewMultiDBManager(baseOpts, []int{0, 1})
	defer mgr.Close()

	client, err := mgr.GetClient(0)
	if err != nil {
		t.Fatalf("failed to get client 0: %v", err)
	}

	opts := client.Options()
	if opts.PoolSize != 25 {
		t.Errorf("expected PoolSize 25, got %d", opts.PoolSize)
	}
	if opts.MinIdleConns != 8 {
		t.Errorf("expected MinIdleConns 8, got %d", opts.MinIdleConns)
	}
	if opts.ConnMaxIdleTime != 10*time.Minute {
		t.Errorf("expected ConnMaxIdleTime 10m, got %v", opts.ConnMaxIdleTime)
	}
	if opts.ConnMaxLifetime != 1*time.Hour {
		t.Errorf("expected ConnMaxLifetime 1h, got %v", opts.ConnMaxLifetime)
	}
}

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

const twoMySQLConfig = `
server:
  port: 8080
auth:
  enabled: false
  resource_url: "${MCP_RESOURCE_URL:-http://127.0.0.1:8080}"
sources:
  mysql:
    orders:
      dsn: "${MYSQL_ORDERS_DSN:-orders-dsn}"
      security:
        max_limit: 50
    billing:
      dsn: "${MYSQL_BILLING_DSN:-billing-dsn}"
  redis:
    cache:
      addr: "${REDIS_ADDR:-127.0.0.1:6379}"
      password: "${REDIS_PASSWORD:-}"
endpoints:
  - path: /mysql1
    sources: [orders]
  - path: /mysql2
    sources: [billing]
  - path: /redis
    sources: [cache]
`

func TestLoadTwoMySQLEndpoints(t *testing.T) {
	t.Setenv("MYSQL_ORDERS_DSN", "orders-from-env")
	t.Setenv("MYSQL_BILLING_DSN", "")

	cfg, err := Load(writeConfig(t, twoMySQLConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := cfg.Sources.MySQL["orders"].DSN; got != "orders-from-env" {
		t.Errorf("orders DSN = %q, want env value", got)
	}
	if got := cfg.Sources.MySQL["billing"].DSN; got != "billing-dsn" {
		t.Errorf("billing DSN = %q, want default", got)
	}
	// 未写的安全上限回落到默认值，写了的保持原值。
	if got := cfg.Sources.MySQL["orders"].Security.MaxLimit; got != 50 {
		t.Errorf("orders max_limit = %d, want 50", got)
	}
	if got := cfg.Sources.MySQL["billing"].Security.MaxLimit; got != 200 {
		t.Errorf("billing max_limit = %d, want default 200", got)
	}
	if got := cfg.Sources.MySQL["orders"].Security.MaxOffset; got != 100000 {
		t.Errorf("orders max_offset = %d, want default 100000", got)
	}
	if got := cfg.Sources.Redis["cache"].PoolSize; got != 20 {
		t.Errorf("redis pool_size = %d, want default 20", got)
	}

	if len(cfg.Endpoints) != 3 {
		t.Fatalf("endpoints = %d, want 3", len(cfg.Endpoints))
	}
	ref, ok := cfg.Lookup("billing")
	if !ok || ref.Kind != SourceKindMySQL || ref.MySQL.DSN != "billing-dsn" {
		t.Errorf("Lookup(billing) = %+v, ok %v", ref, ok)
	}
	if _, ok := cfg.Lookup("missing"); ok {
		t.Error("Lookup(missing) should fail")
	}
}

func TestLoadRejects(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		errPart string
	}{
		{
			name: "duplicate source name across kinds",
			body: `
sources:
  mysql:
    main:
      dsn: a
  redis:
    main:
      addr: b
endpoints:
  - path: /db
    sources: [main]
`,
			errPart: "declared as both",
		},
		{
			name: "unknown source",
			body: `
sources:
  mysql:
    orders:
      dsn: a
endpoints:
  - path: /mysql1
    sources: [billing]
`,
			errPart: "unknown source",
		},
		{
			name: "duplicate endpoint path",
			body: `
sources:
  mysql:
    orders:
      dsn: a
    billing:
      dsn: b
endpoints:
  - path: /mysql
    sources: [orders]
  - path: /mysql
    sources: [billing]
`,
			errPart: "duplicate path",
		},
		{
			name: "nested path",
			body: `
sources:
  mysql:
    orders:
      dsn: a
endpoints:
  - path: /db/mysql
    sources: [orders]
`,
			errPart: "single segment",
		},
		{
			name: "reserved path",
			body: `
sources:
  mysql:
    orders:
      dsn: a
endpoints:
  - path: /health
    sources: [orders]
`,
			errPart: "reserved",
		},
		{
			name: "no endpoints",
			body: `
sources:
  mysql:
    orders:
      dsn: a
`,
			errPart: "at least one endpoint",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.body))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.errPart) {
				t.Errorf("error %q does not contain %q", err, tc.errPart)
			}
		})
	}
}

func TestLoadRedisACLAndCasdoorConfig(t *testing.T) {
	t.Setenv("REDIS_USER", "mcp_ro")
	t.Setenv("CASDOOR_AUD", "f983ca0f75f64773a09c")
	t.Setenv("CASDOOR_JWKS", "https://casdoor.example.com/.well-known/jwks")

	raw := `
server:
  port: 8080
auth:
  enabled: true
  client_id: "mcp-service"
  audience: "${CASDOOR_AUD:-default-aud}"
  jwks_uri: "${CASDOOR_JWKS:-}"
sources:
  mysql:
    db1:
      dsn: "root:pass@tcp(127.0.0.1:3306)/test"
  redis:
    cache:
      addr: "127.0.0.1:6379"
      username: "${REDIS_USER:-default-user}"
      password: "pwd"
endpoints:
  - path: /db1
    sources: [db1]
  - path: /redis
    sources: [cache]
`
	cfg, err := Load(writeConfig(t, raw))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := cfg.Sources.Redis["cache"].Username; got != "mcp_ro" {
		t.Errorf("redis username = %q, want 'mcp_ro'", got)
	}
	if got := cfg.Auth.Audience; got != "f983ca0f75f64773a09c" {
		t.Errorf("auth audience = %q, want 'f983ca0f75f64773a09c'", got)
	}
	if got := cfg.Auth.JWKSURI; got != "https://casdoor.example.com/.well-known/jwks" {
		t.Errorf("auth jwks_uri = %q, want 'https://casdoor.example.com/.well-known/jwks'", got)
	}
}

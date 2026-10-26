package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"echoa/internal/config"
)

func TestOAuthProtectedResourceFlow(t *testing.T) {
	cfg := config.AuthConfig{
		Enabled:        true,
		ResourceURL:    "http://127.0.0.1:18091",
		Endpoint:       "https://casdoor.example.com",
		ScopesRequired: []string{"openid", "offline_access"},
	}

	h := server.Default(server.WithHostPorts("127.0.0.1:18091"))

	// 挂载 PRM 接口
	h.GET("/.well-known/oauth-protected-resource", ProtectedResourceMetadataHandler(cfg))

	// 挂载受保护接口
	authMw := CasdoorMiddleware(cfg, nil)
	h.GET("/sse", authMw, func(ctx context.Context, c *app.RequestContext) {
		c.Status(consts.StatusOK)
	})

	go h.Spin()
	time.Sleep(200 * time.Millisecond)

	// 1. 测试直接请求受保护接口，未携带 Token -> 必须返回 401 + WWW-Authenticate 头
	resp, err := http.Get("http://127.0.0.1:18091/sse")
	if err != nil {
		t.Fatalf("failed to request /sse: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected status 401 Unauthorized, got %d", resp.StatusCode)
	}

	authHeader := resp.Header.Get("WWW-Authenticate")
	t.Logf("Received WWW-Authenticate header: %s", authHeader)
	if !strings.Contains(authHeader, `resource_metadata="http://127.0.0.1:18091/.well-known/oauth-protected-resource"`) {
		t.Fatalf("expected resource_metadata in WWW-Authenticate, got: %s", authHeader)
	}
	if !strings.Contains(authHeader, `scope="openid offline_access"`) {
		t.Fatalf("expected scope in WWW-Authenticate, got: %s", authHeader)
	}

	// 2. 模拟客户端顺着指示访问 PRM 元数据接口
	prmResp, err := http.Get("http://127.0.0.1:18091/.well-known/oauth-protected-resource")
	if err != nil {
		t.Fatalf("failed to request PRM: %v", err)
	}
	defer prmResp.Body.Close()

	if prmResp.StatusCode != http.StatusOK {
		t.Fatalf("expected PRM status 200, got %d", prmResp.StatusCode)
	}

	var metadata ProtectedResourceMetadata
	if err := json.NewDecoder(prmResp.Body).Decode(&metadata); err != nil {
		t.Fatalf("failed to decode PRM metadata: %v", err)
	}

	t.Logf("Decoded PRM metadata: %+v", metadata)
	if metadata.Resource != "http://127.0.0.1:18091" {
		t.Errorf("expected resource %s, got %s", "http://127.0.0.1:18091", metadata.Resource)
	}
	if len(metadata.AuthorizationServers) == 0 || metadata.AuthorizationServers[0] != "https://casdoor.example.com" {
		t.Errorf("expected authorization server 'https://casdoor.example.com', got %+v", metadata.AuthorizationServers)
	}
}

func TestSanitizeToken(t *testing.T) {
	if got := SanitizeToken(""); got != "" {
		t.Errorf("expected empty string, got: %s", got)
	}
	if got := SanitizeToken("short"); got != "[REDACTED]" {
		t.Errorf("expected [REDACTED], got: %s", got)
	}
	longToken := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.dummy_body.signature1234"
	got := SanitizeToken(longToken)
	if !strings.HasPrefix(got, "eyJh...") || !strings.HasSuffix(got, "...1234") {
		t.Errorf("expected masked token eyJh...1234, got: %s", got)
	}
}

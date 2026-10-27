package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"echoa/internal/config"
)

func TestOAuthProtectedResourceFlow(t *testing.T) {
	cfg := config.AuthConfig{
		Enabled:        true,
		Endpoint:       "https://casdoor.example.com",
		ScopesRequired: []string{"openid", "offline_access"},
	}

	// 先占一个端口确定资源地址：PRM 响应体和 WWW-Authenticate 都在 handler 创建时固定下来
	listener := httptest.NewUnstartedServer(nil)
	cfg.ResourceURL = "http://" + listener.Listener.Addr().String()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", ProtectedResourceMetadataHandler(cfg))
	mux.Handle("GET /sse", CasdoorMiddleware(cfg, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	listener.Config.Handler = mux
	listener.Start()
	defer listener.Close()
	srv := listener

	// 1. 测试直接请求受保护接口，未携带 Token -> 必须返回 401 + WWW-Authenticate 头
	resp, err := http.Get(srv.URL + "/sse")
	if err != nil {
		t.Fatalf("failed to request /sse: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected status 401 Unauthorized, got %d", resp.StatusCode)
	}

	authHeader := resp.Header.Get("WWW-Authenticate")
	t.Logf("Received WWW-Authenticate header: %s", authHeader)
	if !strings.Contains(authHeader, `resource_metadata="`+cfg.ResourceURL+`/.well-known/oauth-protected-resource"`) {
		t.Fatalf("expected resource_metadata in WWW-Authenticate, got: %s", authHeader)
	}
	if !strings.Contains(authHeader, `scope="openid offline_access"`) {
		t.Fatalf("expected scope in WWW-Authenticate, got: %s", authHeader)
	}

	// 2. 模拟客户端顺着指示访问 PRM 元数据接口
	prmResp, err := http.Get(srv.URL + "/.well-known/oauth-protected-resource")
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
	if metadata.Resource != cfg.ResourceURL {
		t.Errorf("expected resource %s, got %s", cfg.ResourceURL, metadata.Resource)
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

package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"echoa/internal/auth"
	"echoa/internal/config"
	"github.com/casdoor/casdoor-go-sdk/casdoorsdk"
)

// newTestServer 启动一个挂载了 MCP 路由、鉴权关闭的测试服务器。
func newTestServer(t *testing.T, mcpServer *Server) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	authMw := auth.CasdoorMiddleware(config.AuthConfig{Enabled: false}, nil)
	mux.Handle("GET /sse", authMw(http.HandlerFunc(mcpServer.HandleSSE)))
	mux.Handle("POST /message", authMw(http.HandlerFunc(mcpServer.HandleMessage)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestMCPEndToEnd(t *testing.T) {
	// 1. 初始化 ToolRegistry 并注册模拟工具
	registry := NewToolRegistry()

	registry.Register(&ToolDefinition{
		Name:        "test_ping",
		Description: "A ping test tool",
		InputSchema: map[string]any{"type": "object"},
		Handler: func(ctx context.Context, claims *casdoorsdk.Claims, args map[string]any) (string, error) {
			return "pong", nil
		},
	})

	mcpServer := NewServer(registry)
	defer mcpServer.Close()

	// 2. 启动测试服务器
	srv := newTestServer(t, mcpServer)

	// 3. 发起 GET /sse 请求
	req, err := http.NewRequest("GET", srv.URL+"/sse", nil)
	if err != nil {
		t.Fatalf("failed to create SSE request: %v", err)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("SSE request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	reader := bufio.NewReader(resp.Body)

	// 4. 读取首个 endpoint 事件
	var endpointURL string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("error reading SSE: %v", err)
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") {
			endpointURL = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			break
		}
	}

	if !strings.HasPrefix(endpointURL, "/message?sessionId=") {
		t.Fatalf("unexpected endpoint URL: %s", endpointURL)
	}
	t.Logf("Received endpoint URL: %s", endpointURL)

	// 5. 发送 POST /message (initialize)
	postURL := srv.URL + endpointURL
	initPayload := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{},
	}
	body, _ := json.Marshal(initPayload)
	postResp, err := http.Post(postURL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST initialize failed: %v", err)
	}
	defer postResp.Body.Close()

	if postResp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected status 202 Accepted, got %d", postResp.StatusCode)
	}

	// 6. 从 SSE 读取 initialize 返回结果
	var initResult string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("error reading SSE init response: %v", err)
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") {
			initResult = strings.TrimPrefix(line, "data:")
			break
		}
	}

	t.Logf("Received JSON-RPC response: %s", initResult)
	if !strings.Contains(initResult, "echoa") {
		t.Fatalf("expected serverInfo 'echoa' in response, got %s", initResult)
	}

	// 7. 发送 POST /message (tools/list)
	toolsListPayload := map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
		"params":  map[string]any{},
	}
	body, _ = json.Marshal(toolsListPayload)
	postResp2, err := http.Post(postURL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST tools/list failed: %v", err)
	}
	defer postResp2.Body.Close()

	var toolsListResult string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("error reading SSE tools/list response: %v", err)
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") {
			toolsListResult = strings.TrimPrefix(line, "data:")
			break
		}
	}
	t.Logf("Received tools/list response: %s", toolsListResult)
	if !strings.Contains(toolsListResult, "test_ping") {
		t.Fatalf("expected test_ping in tools list, got %s", toolsListResult)
	}

	// 8. 发送 POST /message (tools/call test_ping)
	callPayload := map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "test_ping",
			"arguments": map[string]any{},
		},
	}
	body, _ = json.Marshal(callPayload)
	postResp3, err := http.Post(postURL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST tools/call failed: %v", err)
	}
	defer postResp3.Body.Close()

	var callResult string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("error reading SSE tools/call response: %v", err)
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") {
			callResult = strings.TrimPrefix(line, "data:")
			break
		}
	}
	t.Logf("Received tools/call response: %s", callResult)
	if !strings.Contains(callResult, "pong") {
		t.Fatalf("expected 'pong' in result, got %s", callResult)
	}
}

func TestSessionConcurrencyLimit(t *testing.T) {
	registry := NewToolRegistry()

	// 注册一个执行 300ms 的慢工具
	registry.Register(&ToolDefinition{
		Name:        "test_slow",
		Description: "A slow test tool",
		InputSchema: map[string]any{"type": "object"},
		Handler: func(ctx context.Context, claims *casdoorsdk.Claims, args map[string]any) (string, error) {
			time.Sleep(300 * time.Millisecond)
			return "done", nil
		},
	})

	mcpServer := NewServer(registry)
	defer mcpServer.Close()

	srv := newTestServer(t, mcpServer)

	// 1. 建立 SSE 连接
	req, err := http.NewRequest("GET", srv.URL+"/sse", nil)
	if err != nil {
		t.Fatalf("failed to create SSE request: %v", err)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("SSE request failed: %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	var endpointURL string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("error reading SSE: %v", err)
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") {
			endpointURL = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			break
		}
	}

	postURL := srv.URL + endpointURL

	// 2. 同时发起 5 个慢任务占用全部并发槽位 (MaxSessionConcurrency = 5)
	var wg sync.WaitGroup
	var acceptedCount int
	var countMu sync.Mutex

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			payload := map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"method":  "tools/call",
				"params": map[string]any{
					"name":      "test_slow",
					"arguments": map[string]any{},
				},
			}
			b, _ := json.Marshal(payload)
			pResp, pErr := http.Post(postURL, "application/json", bytes.NewReader(b))
			if pErr == nil {
				defer pResp.Body.Close()
				if pResp.StatusCode == http.StatusAccepted {
					countMu.Lock()
					acceptedCount++
					countMu.Unlock()
				}
			}
		}(i + 10)
	}

	// 稍微等待确保 5 个请求已进入执行中
	time.Sleep(50 * time.Millisecond)

	// 3. 发送第 6 个请求 -> 槽位已满，必须返回 429 Too Many Requests
	sixthPayload := map[string]any{
		"jsonrpc": "2.0",
		"id":      99,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "test_slow",
			"arguments": map[string]any{},
		},
	}
	sixthBody, _ := json.Marshal(sixthPayload)
	sixthResp, err := http.Post(postURL, "application/json", bytes.NewReader(sixthBody))
	if err != nil {
		t.Fatalf("sixth request failed: %v", err)
	}
	defer sixthResp.Body.Close()

	if sixthResp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 6th request to be 429 Too Many Requests, got: %d", sixthResp.StatusCode)
	}

	var errResp map[string]any
	_ = json.NewDecoder(sixthResp.Body).Decode(&errResp)
	t.Logf("Received expected 429 response: %+v", errResp)

	wg.Wait()
	if acceptedCount != 5 {
		t.Errorf("expected 5 requests to be accepted, got: %d", acceptedCount)
	}
	// 等待 5 个后台 goroutine 顺利执行完
	time.Sleep(350 * time.Millisecond)
}

func TestSanitizeAuditArgs(t *testing.T) {
	// 1. 测试空入参
	emptyResult := sanitizeAuditArgs(nil)
	if len(emptyResult) != 0 {
		t.Errorf("expected empty map, got %v", emptyResult)
	}

	// 2. 测试敏感 Key 脱敏与大文本截断
	longQuery := strings.Repeat("SELECT * FROM users WHERE id = 1 AND ", 10) // > 200 字符
	rawArgs := map[string]any{
		"query":      longQuery,
		"password":   "super_secret_123",
		"Token":      "jwt-token-string",
		"SECRET":     "my_secret",
		"Credential": "secret_cred",
		"auth":       "bearer token",
		"api_key":    "not_exact_key",
		"key":        "sensitive_key_val",
		"max_rows":   100,
		"is_active":  true,
	}

	sanitized := sanitizeAuditArgs(rawArgs)

	// 验证敏感字段脱敏
	for _, k := range []string{"password", "Token", "SECRET", "Credential", "auth", "key"} {
		if val, ok := sanitized[k]; !ok || val != "[REDACTED]" {
			t.Errorf("expected key %s to be [REDACTED], got %v", k, val)
		}
	}

	// 验证普通安全字段未脱敏
	if sanitized["max_rows"] != 100 {
		t.Errorf("expected max_rows to be 100, got %v", sanitized["max_rows"])
	}
	if sanitized["is_active"] != true {
		t.Errorf("expected is_active to be true, got %v", sanitized["is_active"])
	}

	// 验证超长文本截断
	qVal, ok := sanitized["query"].(string)
	if !ok {
		t.Fatalf("expected query to be string, got %T", sanitized["query"])
	}
	if !strings.HasSuffix(qVal, "... [truncated]") {
		t.Errorf("expected truncated string to end with '... [truncated]', got: %s", qVal)
	}
	if len(qVal) != 200+len("... [truncated]") {
		t.Errorf("unexpected length for truncated query: %d", len(qVal))
	}
}

func TestResolveEndpointURL(t *testing.T) {
	serverNoResource := NewServer(nil)
	serverWithResource := NewServer(nil, "https://mcp.internal.example.com/prefix")

	sessionID := "test-session-uuid-123"

	// 1. 无代理头，无 resourceURL -> 兜底相对路径
	req1, _ := http.NewRequest("GET", "/sse", nil)
	url1 := serverNoResource.resolveEndpointURL(req1, sessionID)
	expected1 := "/message?sessionId=" + sessionID
	if url1 != expected1 {
		t.Errorf("case 1 failed: expected %s, got %s", expected1, url1)
	}

	// 2. 有 resourceURL，无代理头 -> 使用 resourceURL 前缀
	req2, _ := http.NewRequest("GET", "/sse", nil)
	url2 := serverWithResource.resolveEndpointURL(req2, sessionID)
	expected2 := "https://mcp.internal.example.com/prefix/message?sessionId=" + sessionID
	if url2 != expected2 {
		t.Errorf("case 2 failed: expected %s, got %s", expected2, url2)
	}

	// 3. 有 resourceURL（带尾部斜杠），验证自动 TrimRight 斜杠
	serverTrailingSlash := NewServer(nil, "https://mcp.internal.example.com/")
	url3 := serverTrailingSlash.resolveEndpointURL(req2, sessionID)
	expected3 := "https://mcp.internal.example.com/message?sessionId=" + sessionID
	if url3 != expected3 {
		t.Errorf("case 3 failed: expected %s, got %s", expected3, url3)
	}

	// 4. 有 X-Forwarded-Prefix 头 -> 优先使用 X-Forwarded-Prefix
	req4, _ := http.NewRequest("GET", "/sse", nil)
	req4.Header.Set("X-Forwarded-Prefix", "/api/v1/mcp/")
	url4 := serverWithResource.resolveEndpointURL(req4, sessionID)
	expected4 := "/api/v1/mcp/message?sessionId=" + sessionID
	if url4 != expected4 {
		t.Errorf("case 4 failed: expected %s, got %s", expected4, url4)
	}
}

func TestHealthEndpoints(t *testing.T) {
	mux := http.NewServeMux()

	checkReadiness := func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":      "ok",
			"service":     "echoa",
			"datasources": map[string]any{"mock_db": "ok"},
		})
	}

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("type") == "ready" {
			checkReadiness(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "echoa"})
	})
	mux.HandleFunc("GET /health/ready", checkReadiness)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 1. 测试基础 Liveness /health
	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK, got %d", resp.StatusCode)
	}
	var livenessBody map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&livenessBody)
	if livenessBody["status"] != "ok" || livenessBody["service"] != "echoa" {
		t.Errorf("unexpected liveness body: %+v", livenessBody)
	}

	// 2. 测试 Readiness /health/ready
	respReady, err := http.Get(srv.URL + "/health/ready")
	if err != nil {
		t.Fatalf("GET /health/ready failed: %v", err)
	}
	defer respReady.Body.Close()
	if respReady.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK, got %d", respReady.StatusCode)
	}
	var readyBody map[string]any
	_ = json.NewDecoder(respReady.Body).Decode(&readyBody)
	if readyBody["status"] != "ok" {
		t.Errorf("unexpected readiness status: %+v", readyBody)
	}
	ds, ok := readyBody["datasources"].(map[string]any)
	if !ok || ds["mock_db"] != "ok" {
		t.Errorf("unexpected datasources: %+v", readyBody)
	}

	// 3. 测试 /health?type=ready
	respQueryReady, err := http.Get(srv.URL + "/health?type=ready")
	if err != nil {
		t.Fatalf("GET /health?type=ready failed: %v", err)
	}
	defer respQueryReady.Body.Close()
	if respQueryReady.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK, got %d", respQueryReady.StatusCode)
	}
}

func TestSessionSendTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 创建一个缓冲区仅为 1 的测试 Session
	sess := &Session{
		ID:       "test-timeout-sess",
		SendChan: make(chan *Event, 1),
		Sem:      make(chan struct{}, 5),
		LastSeen: time.Now(),
		Ctx:      ctx,
		Cancel:   cancel,
	}

	// 1. 发送第 1 个包，填满缓冲区
	err := sess.SendJSONRPCWithTimeout(1, "payload1", nil, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("first send should succeed, got: %v", err)
	}

	// 2. 缓冲区已满且无人消费，发送第 2 个包应在 50ms 超时熔断，不能永久挂死协程
	start := time.Now()
	err = sess.SendJSONRPCWithTimeout(2, "payload2", nil, 50*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("second send on full buffer should fail with timeout error")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("expected timeout error message, got: %v", err)
	}
	if elapsed < 40*time.Millisecond || elapsed > 300*time.Millisecond {
		t.Errorf("timeout should trigger around 50ms, elapsed: %v", elapsed)
	}
}

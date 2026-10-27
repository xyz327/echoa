package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"echoa/internal/auth"
	"echoa/internal/config"
	"github.com/casdoor/casdoor-go-sdk/casdoorsdk"
)

func newTestServer(t *testing.T, srv *Server) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	authMw := auth.CasdoorMiddleware(config.AuthConfig{Enabled: false}, nil)
	mux.Handle(srv.endpointPath, authMw(srv))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func standardHeaders(method, toolName string) map[string]string {
	h := map[string]string{
		"Content-Type":         "application/json",
		"MCP-Protocol-Version": ProtocolVersion20260728,
		"Mcp-Method":           method,
	}
	if toolName != "" {
		h["Mcp-Name"] = toolName
	}
	return h
}

func standardBody(id any, method string, params map[string]any) map[string]any {
	if params == nil {
		params = map[string]any{}
	}
	if _, ok := params["_meta"]; !ok {
		params["_meta"] = map[string]any{
			"io.modelcontextprotocol/protocolVersion": ProtocolVersion20260728,
		}
	}
	body := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	}
	if id != nil {
		body["id"] = id
	}
	return body
}

func doPOST(t *testing.T, url string, headers map[string]string, body any) (*http.Response, []byte) {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body error: %v", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequest(http.MethodPost, url, bodyReader)
	if err != nil {
		t.Fatalf("new request error: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do error: %v", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body error: %v", err)
	}
	return resp, respBytes
}

func TestMCPMethods405(t *testing.T) {
	registry := NewToolRegistry()
	srv := NewServer("/mysql", registry, 5, nil)
	ts := newTestServer(t, srv)

	for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodPut} {
		req, _ := http.NewRequest(method, ts.URL+"/mysql", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s request error: %v", method, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("expected 405 for %s, got %d", method, resp.StatusCode)
		}
		if allow := resp.Header.Get("Allow"); allow != "POST" {
			t.Errorf("expected Allow: POST, got %q", allow)
		}
	}
}

func TestMCPOriginValidation(t *testing.T) {
	registry := NewToolRegistry()
	srv := NewServer("/mysql", registry, 5, []string{"https://allowed.example.com"})
	ts := newTestServer(t, srv)

	headers := standardHeaders("server/discover", "")
	body := standardBody(1, "server/discover", nil)

	// 1. 无 Origin：应允许
	resp, _ := doPOST(t, ts.URL+"/mysql", headers, body)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for empty Origin, got %d", resp.StatusCode)
	}

	// 2. 匹配 allowed_origins：应允许
	headers["Origin"] = "https://allowed.example.com"
	resp, _ = doPOST(t, ts.URL+"/mysql", headers, body)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for allowed Origin, got %d", resp.StatusCode)
	}

	// 3. 不匹配 Origin：应返回 403 Forbidden
	headers["Origin"] = "https://malicious.example.com"
	resp, respBytes := doPOST(t, ts.URL+"/mysql", headers, body)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for disallowed Origin, got %d", resp.StatusCode)
	}
	var rpcErr JSONRPCResponse
	_ = json.Unmarshal(respBytes, &rpcErr)
	if rpcErr.Error == nil {
		t.Errorf("expected JSON-RPC error in 403 response, got %s", string(respBytes))
	}
}

func TestMCPHeaderValidation(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register(&ToolDefinition{
		Name:        "test_tool",
		Description: "test tool",
		InputSchema: map[string]any{"type": "object"},
		Handler: func(ctx context.Context, claims *casdoorsdk.Claims, args map[string]any) (string, error) {
			return "ok", nil
		},
	})
	srv := NewServer("/mysql", registry, 5, nil)
	ts := newTestServer(t, srv)

	// 1. 缺少 MCP-Protocol-Version 头 -> 400, code -32020
	h1 := standardHeaders("server/discover", "")
	delete(h1, "MCP-Protocol-Version")
	resp, respBytes := doPOST(t, ts.URL+"/mysql", h1, standardBody(1, "server/discover", nil))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for missing MCP-Protocol-Version, got %d", resp.StatusCode)
	}
	var errResp JSONRPCResponse
	_ = json.Unmarshal(respBytes, &errResp)
	if errResp.Error == nil || errResp.Error.Code != ErrCodeHeaderMismatch {
		t.Errorf("expected error code %d, got %v", ErrCodeHeaderMismatch, errResp.Error)
	}

	// 2. 头部协议版本与 body _meta 不一致 -> 400, code -32020
	h2 := standardHeaders("server/discover", "")
	b2 := standardBody(1, "server/discover", map[string]any{
		"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion": "2025-11-25",
		},
	})
	resp, respBytes = doPOST(t, ts.URL+"/mysql", h2, b2)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for version mismatch, got %d", resp.StatusCode)
	}
	_ = json.Unmarshal(respBytes, &errResp)
	if errResp.Error == nil || errResp.Error.Code != ErrCodeHeaderMismatch {
		t.Errorf("expected error code %d, got %v", ErrCodeHeaderMismatch, errResp.Error)
	}

	// 3. 不支持的协议版本 -> 400, code -32022
	h3 := standardHeaders("server/discover", "")
	h3["MCP-Protocol-Version"] = "2025-11-25"
	b3 := standardBody(1, "server/discover", map[string]any{
		"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion": "2025-11-25",
		},
	})
	resp, respBytes = doPOST(t, ts.URL+"/mysql", h3, b3)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for unsupported version, got %d", resp.StatusCode)
	}
	_ = json.Unmarshal(respBytes, &errResp)
	if errResp.Error == nil || errResp.Error.Code != ErrCodeUnsupportedProtocolVersion {
		t.Errorf("expected error code %d, got %v", ErrCodeUnsupportedProtocolVersion, errResp.Error)
	}

	// 4. 缺少 Mcp-Method 头 -> 400, code -32020
	h4 := standardHeaders("server/discover", "")
	delete(h4, "Mcp-Method")
	resp, _ = doPOST(t, ts.URL+"/mysql", h4, standardBody(1, "server/discover", nil))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for missing Mcp-Method, got %d", resp.StatusCode)
	}

	// 5. Mcp-Method 与 body method 不一致 -> 400, code -32020
	h5 := standardHeaders("server/discover", "")
	h5["Mcp-Method"] = "tools/list"
	resp, _ = doPOST(t, ts.URL+"/mysql", h5, standardBody(1, "server/discover", nil))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for Mcp-Method mismatch, got %d", resp.StatusCode)
	}

	// 6. tools/call 缺少 Mcp-Name -> 400, code -32020
	h6 := standardHeaders("tools/call", "")
	b6 := standardBody(1, "tools/call", map[string]any{
		"name":      "test_tool",
		"arguments": map[string]any{},
	})
	resp, _ = doPOST(t, ts.URL+"/mysql", h6, b6)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for missing Mcp-Name on tools/call, got %d", resp.StatusCode)
	}

	// 7. tools/call Mcp-Name 不匹配 -> 400, code -32020
	h7 := standardHeaders("tools/call", "wrong_tool")
	resp, _ = doPOST(t, ts.URL+"/mysql", h7, b6)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for mismatched Mcp-Name, got %d", resp.StatusCode)
	}

	// 8. tools/call 采用 Base64 sentinel 编码 Mcp-Name -> 应成功解码并匹配执行
	encodedName := "=?base64?" + base64.StdEncoding.EncodeToString([]byte("test_tool")) + "?="
	h8 := standardHeaders("tools/call", encodedName)
	resp, _ = doPOST(t, ts.URL+"/mysql", h8, b6)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for Base64 encoded Mcp-Name, got %d", resp.StatusCode)
	}

	// 9. 携带旧版 Mcp-Session-Id 头 -> 应忽略并不影响执行
	h9 := standardHeaders("tools/call", "test_tool")
	h9["Mcp-Session-Id"] = "legacy-session-uuid"
	resp, _ = doPOST(t, ts.URL+"/mysql", h9, b6)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 when Mcp-Session-Id is present, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Mcp-Session-Id") != "" {
		t.Errorf("server must not echo or mint Mcp-Session-Id header")
	}
}

func TestMCPNotification(t *testing.T) {
	registry := NewToolRegistry()
	srv := NewServer("/mysql", registry, 5, nil)
	ts := newTestServer(t, srv)

	// 没有 id 的请求视为 Notification -> 202 Accepted 无包体
	headers := map[string]string{"Content-Type": "application/json"}
	body := map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	}
	resp, respBytes := doPOST(t, ts.URL+"/mysql", headers, body)
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("expected 202 Accepted for notification, got %d", resp.StatusCode)
	}
	if len(respBytes) > 0 {
		t.Errorf("expected empty body for 202 Accepted, got %s", string(respBytes))
	}
}

func TestServerDiscover(t *testing.T) {
	registry := NewToolRegistry()
	srv := NewServer("/mysql", registry, 5, nil)
	ts := newTestServer(t, srv)

	headers := standardHeaders("server/discover", "")
	body := standardBody("disc-1", "server/discover", nil)

	resp, respBytes := doPOST(t, ts.URL+"/mysql", headers, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var rpcResp struct {
		JSONRPC string         `json:"jsonrpc"`
		ID      any            `json:"id"`
		Result  DiscoverResult `json:"result"`
	}
	if err := json.Unmarshal(respBytes, &rpcResp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if rpcResp.Result.ResultType != "complete" {
		t.Errorf("expected resultType 'complete', got %q", rpcResp.Result.ResultType)
	}
	if len(rpcResp.Result.SupportedVersions) != 1 || rpcResp.Result.SupportedVersions[0] != ProtocolVersion20260728 {
		t.Errorf("expected supportedVersions [2026-07-28], got %v", rpcResp.Result.SupportedVersions)
	}
	if rpcResp.Result.TTLMs <= 0 || rpcResp.Result.CacheScope != "public" {
		t.Errorf("expected cache hints, got ttlMs=%d, cacheScope=%s", rpcResp.Result.TTLMs, rpcResp.Result.CacheScope)
	}
}

func TestToolsList(t *testing.T) {
	registry := NewToolRegistry()
	// 乱序注册，验证 tools/list 字典序稳定输出
	registry.Register(&ToolDefinition{Name: "zebra_tool", Description: "z"})
	registry.Register(&ToolDefinition{Name: "apple_tool", Description: "a"})
	registry.Register(&ToolDefinition{Name: "monkey_tool", Description: "m"})

	srv := NewServer("/mysql", registry, 5, nil)
	ts := newTestServer(t, srv)

	headers := standardHeaders("tools/list", "")
	body := standardBody(10, "tools/list", nil)

	resp, respBytes := doPOST(t, ts.URL+"/mysql", headers, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var rpcResp struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      any             `json:"id"`
		Result  ListToolsResult `json:"result"`
	}
	if err := json.Unmarshal(respBytes, &rpcResp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if rpcResp.Result.ResultType != "complete" {
		t.Errorf("expected resultType 'complete', got %s", rpcResp.Result.ResultType)
	}
	if len(rpcResp.Result.Tools) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(rpcResp.Result.Tools))
	}
	// 验证排序：apple_tool, monkey_tool, zebra_tool
	expectedNames := []string{"apple_tool", "monkey_tool", "zebra_tool"}
	for i, name := range expectedNames {
		if rpcResp.Result.Tools[i].Name != name {
			t.Errorf("tool[%d] name expected %s, got %s", i, name, rpcResp.Result.Tools[i].Name)
		}
	}
}

func TestToolsCall(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register(&ToolDefinition{
		Name:        "echo_tool",
		Description: "echoes message",
		InputSchema: map[string]any{"type": "object"},
		Handler: func(ctx context.Context, claims *casdoorsdk.Claims, args map[string]any) (string, error) {
			msg, _ := args["message"].(string)
			if msg == "fail" {
				return "", fmt.Errorf("intentional error")
			}
			return "echo: " + msg, nil
		},
	})

	srv := NewServer("/mysql", registry, 5, nil)
	ts := newTestServer(t, srv)

	// 1. 成功调用
	headers := standardHeaders("tools/call", "echo_tool")
	body := standardBody("call-1", "tools/call", map[string]any{
		"name": "echo_tool",
		"arguments": map[string]any{
			"message":  "hello world",
			"password": "secret-password", // 脱敏测试
		},
	})

	resp, respBytes := doPOST(t, ts.URL+"/mysql", headers, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var rpcResp struct {
		JSONRPC string         `json:"jsonrpc"`
		ID      any            `json:"id"`
		Result  CallToolResult `json:"result"`
	}
	if err := json.Unmarshal(respBytes, &rpcResp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if rpcResp.Result.ResultType != "complete" || rpcResp.Result.IsError {
		t.Errorf("expected success complete result, got %+v", rpcResp.Result)
	}
	if len(rpcResp.Result.Content) == 0 || rpcResp.Result.Content[0].Text != "echo: hello world" {
		t.Errorf("unexpected content: %+v", rpcResp.Result.Content)
	}

	// 2. 工具内部报错 -> HTTP 200, Result.IsError = true (不作为 JSON-RPC 协议错误)
	bodyFail := standardBody("call-2", "tools/call", map[string]any{
		"name": "echo_tool",
		"arguments": map[string]any{
			"message": "fail",
		},
	})
	resp, respBytes = doPOST(t, ts.URL+"/mysql", headers, bodyFail)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	_ = json.Unmarshal(respBytes, &rpcResp)
	if !rpcResp.Result.IsError {
		t.Errorf("expected isError: true on tool failure, got false")
	}

	// 3. 调用不存在的工具
	hNotExist := standardHeaders("tools/call", "not_exist")
	bodyNotExist := standardBody("call-3", "tools/call", map[string]any{
		"name":      "not_exist",
		"arguments": map[string]any{},
	})
	resp, respBytes = doPOST(t, ts.URL+"/mysql", hNotExist, bodyNotExist)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	_ = json.Unmarshal(respBytes, &rpcResp)
	if !rpcResp.Result.IsError {
		t.Errorf("expected isError: true for not found tool")
	}
}

func TestUnknownMethod404(t *testing.T) {
	registry := NewToolRegistry()
	srv := NewServer("/mysql", registry, 5, nil)
	ts := newTestServer(t, srv)

	// 未实现的方法应返回 404 Method not found (-32601)
	headers := standardHeaders("unknown/method", "")
	body := standardBody(1, "unknown/method", nil)

	resp, respBytes := doPOST(t, ts.URL+"/mysql", headers, body)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for unknown method, got %d", resp.StatusCode)
	}

	var rpcErr JSONRPCResponse
	_ = json.Unmarshal(respBytes, &rpcErr)
	if rpcErr.Error == nil || rpcErr.Error.Code != ErrCodeMethodNotFound {
		t.Errorf("expected code %d, got %v", ErrCodeMethodNotFound, rpcErr.Error)
	}
}

func TestStandardClientLifecycle(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register(&ToolDefinition{
		Name:        "orders_query",
		Description: "query orders",
		InputSchema: map[string]any{"type": "object"},
		Handler: func(ctx context.Context, claims *casdoorsdk.Claims, args map[string]any) (string, error) {
			return `[{"id": 101, "total": 99.9}]`, nil
		},
	})
	srv := NewServer("/mysql", registry, 5, nil)
	ts := newTestServer(t, srv)

	// 模拟 Cursor / Claude Desktop / Cline 客户端，无需 Mcp-Method / Mcp-Name / _meta 标头
	plainHeaders := map[string]string{
		"Content-Type": "application/json",
	}

	// 1. initialize 握手阶段
	initBody := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "cursor",
				"version": "0.45.0",
			},
		},
	}
	resp, respBytes := doPOST(t, ts.URL+"/mysql", plainHeaders, initBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for initialize, got %d: %s", resp.StatusCode, string(respBytes))
	}
	var initResp struct {
		JSONRPC string           `json:"jsonrpc"`
		ID      any              `json:"id"`
		Result  InitializeResult `json:"result"`
	}
	if err := json.Unmarshal(respBytes, &initResp); err != nil {
		t.Fatalf("unmarshal initialize error: %v", err)
	}
	if initResp.Result.ProtocolVersion != "2024-11-05" {
		t.Errorf("expected protocolVersion 2024-11-05, got %q", initResp.Result.ProtocolVersion)
	}
	if initResp.Result.ServerInfo.Name != "echoa" {
		t.Errorf("expected server name echoa, got %q", initResp.Result.ServerInfo.Name)
	}
	if protoHeader := resp.Header.Get("MCP-Protocol-Version"); protoHeader != "2024-11-05" {
		t.Errorf("expected MCP-Protocol-Version 2024-11-05 in header, got %q", protoHeader)
	}

	// 2. notifications/initialized 通知
	notifBody := map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	}
	respNotif, respNotifBytes := doPOST(t, ts.URL+"/mysql", plainHeaders, notifBody)
	if respNotif.StatusCode != http.StatusAccepted {
		t.Errorf("expected 202 Accepted for notification, got %d", respNotif.StatusCode)
	}
	if len(respNotifBytes) > 0 {
		t.Errorf("expected empty body for 202, got %s", string(respNotifBytes))
	}

	// 3. ping 保活
	pingBody := map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "ping",
	}
	respPing, respPingBytes := doPOST(t, ts.URL+"/mysql", plainHeaders, pingBody)
	if respPing.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for ping, got %d", respPing.StatusCode)
	}
	var pingResp JSONRPCResponse
	if err := json.Unmarshal(respPingBytes, &pingResp); err != nil {
		t.Fatalf("unmarshal ping error: %v", err)
	}
	if pingResp.Error != nil {
		t.Errorf("ping returned error: %v", pingResp.Error)
	}

	// 4. tools/list 获取工具列表 (无 Mcp-Method 头，无 _meta 字段)
	listBody := map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/list",
	}
	respList, respListBytes := doPOST(t, ts.URL+"/mysql", plainHeaders, listBody)
	if respList.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for tools/list, got %d: %s", respList.StatusCode, string(respListBytes))
	}
	var listResp struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      any             `json:"id"`
		Result  ListToolsResult `json:"result"`
	}
	if err := json.Unmarshal(respListBytes, &listResp); err != nil {
		t.Fatalf("unmarshal tools/list error: %v", err)
	}
	if len(listResp.Result.Tools) != 1 || listResp.Result.Tools[0].Name != "orders_query" {
		t.Errorf("unexpected tools: %+v", listResp.Result.Tools)
	}

	// 5. tools/call 执行工具调用 (无 Mcp-Method / Mcp-Name 头，无 _meta 字段)
	callBody := map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "orders_query",
			"arguments": map[string]any{
				"sql": "SELECT * FROM orders",
			},
		},
	}
	respCall, respCallBytes := doPOST(t, ts.URL+"/mysql", plainHeaders, callBody)
	if respCall.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for tools/call, got %d: %s", respCall.StatusCode, string(respCallBytes))
	}
	var callResp struct {
		JSONRPC string         `json:"jsonrpc"`
		ID      any            `json:"id"`
		Result  CallToolResult `json:"result"`
	}
	if err := json.Unmarshal(respCallBytes, &callResp); err != nil {
		t.Fatalf("unmarshal tools/call error: %v", err)
	}
	if callResp.Result.IsError || len(callResp.Result.Content) == 0 {
		t.Fatalf("unexpected call result: %+v", callResp.Result)
	}
	if callResp.Result.Content[0].Text != `[{"id": 101, "total": 99.9}]` {
		t.Errorf("unexpected content text: %s", callResp.Result.Content[0].Text)
	}
}

func TestStandardClientUnsupportedVersion(t *testing.T) {
	registry := NewToolRegistry()
	srv := NewServer("/mysql", registry, 5, nil)
	ts := newTestServer(t, srv)

	headers := map[string]string{
		"Content-Type":         "application/json",
		"MCP-Protocol-Version": "2020-01-01", // 完全未知的非法版本
	}
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
	}

	resp, respBytes := doPOST(t, ts.URL+"/mysql", headers, body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for unsupported version, got %d", resp.StatusCode)
	}
	var errResp JSONRPCResponse
	_ = json.Unmarshal(respBytes, &errResp)
	if errResp.Error == nil || errResp.Error.Code != ErrCodeUnsupportedProtocolVersion {
		t.Errorf("expected error code %d, got %v", ErrCodeUnsupportedProtocolVersion, errResp.Error)
	}
}

func TestInflightConcurrencyLimit429(t *testing.T) {
	registry := NewToolRegistry()
	holdChan := make(chan struct{})
	startedChan := make(chan struct{}, 10)

	registry.Register(&ToolDefinition{
		Name:        "slow_tool",
		Description: "blocks until holdChan is closed",
		InputSchema: map[string]any{"type": "object"},
		Handler: func(ctx context.Context, claims *casdoorsdk.Claims, args map[string]any) (string, error) {
			startedChan <- struct{}{}
			<-holdChan
			return "done", nil
		},
	})

	maxInflight := 3
	srv := NewServer("/mysql", registry, maxInflight, nil)
	ts := newTestServer(t, srv)

	headers := standardHeaders("tools/call", "slow_tool")
	body := standardBody("req", "tools/call", map[string]any{
		"name":      "slow_tool",
		"arguments": map[string]any{},
	})

	var wg sync.WaitGroup
	for i := 0; i < maxInflight; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			b := standardBody(id, "tools/call", map[string]any{
				"name":      "slow_tool",
				"arguments": map[string]any{},
			})
			resp, _ := doPOST(t, ts.URL+"/mysql", headers, b)
			if resp.StatusCode != http.StatusOK {
				t.Errorf("request %d expected 200, got %d", id, resp.StatusCode)
			}
		}(i)
	}

	// 等待前 3 个慢请求均已进入 handler
	for i := 0; i < maxInflight; i++ {
		select {
		case <-startedChan:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for slow tasks to start")
		}
	}

	// 第 4 个并发请求应被限流阻断，返回 HTTP 429
	b4 := standardBody("overflow", "tools/call", map[string]any{
		"name":      "slow_tool",
		"arguments": map[string]any{},
	})
	resp, respBytes := doPOST(t, ts.URL+"/mysql", headers, b4)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected 429 TooManyRequests for exceeding limit, got %d", resp.StatusCode)
	}
	var rpcErr JSONRPCResponse
	_ = json.Unmarshal(respBytes, &rpcErr)
	if rpcErr.Error == nil || rpcErr.Error.Code != ErrCodeTooManyRequests {
		t.Errorf("expected rpc error %d, got %v", ErrCodeTooManyRequests, rpcErr.Error)
	}

	// 释放阻塞通道，等待前 3 个任务正常结束
	close(holdChan)
	wg.Wait()

	// 槽位归还后，新请求能够正常获取槽位执行
	holdAgain := make(chan struct{})
	close(holdAgain)
	resp2, _ := doPOST(t, ts.URL+"/mysql", headers, body)
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("expected 200 after slots freed, got %d", resp2.StatusCode)
	}
}

func TestEndpointIsolation(t *testing.T) {
	mux := http.NewServeMux()
	authMw := auth.CasdoorMiddleware(config.AuthConfig{Enabled: false}, nil)

	reg1 := NewToolRegistry()
	reg1.Register(&ToolDefinition{Name: "mysql1_tool", Description: "t1"})
	srv1 := NewServer("/mysql1", reg1, 5, nil)
	mux.Handle("/mysql1", authMw(srv1))

	reg2 := NewToolRegistry()
	reg2.Register(&ToolDefinition{Name: "mysql2_tool", Description: "t2"})
	srv2 := NewServer("/mysql2", reg2, 5, nil)
	mux.Handle("/mysql2", authMw(srv2))

	ts := httptest.NewServer(mux)
	defer ts.Close()

	// 1. 请求 /mysql1 只能看到 mysql1_tool
	h1 := standardHeaders("tools/list", "")
	b1 := standardBody(1, "tools/list", nil)
	_, resp1 := doPOST(t, ts.URL+"/mysql1", h1, b1)
	var r1 struct {
		Result ListToolsResult `json:"result"`
	}
	_ = json.Unmarshal(resp1, &r1)
	if len(r1.Result.Tools) != 1 || r1.Result.Tools[0].Name != "mysql1_tool" {
		t.Errorf("unexpected tools on /mysql1: %+v", r1.Result.Tools)
	}

	// 2. 请求 /mysql2 只能看到 mysql2_tool
	_, resp2 := doPOST(t, ts.URL+"/mysql2", h1, b1)
	var r2 struct {
		Result ListToolsResult `json:"result"`
	}
	_ = json.Unmarshal(resp2, &r2)
	if len(r2.Result.Tools) != 1 || r2.Result.Tools[0].Name != "mysql2_tool" {
		t.Errorf("unexpected tools on /mysql2: %+v", r2.Result.Tools)
	}
}

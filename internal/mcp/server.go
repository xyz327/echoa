package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"echoa/internal/auth"
	"github.com/casdoor/casdoor-go-sdk/casdoorsdk"
)

type Server struct {
	endpointPath   string
	toolRegistry   *ToolRegistry
	allowedOrigins []string
	limiter        *InflightLimiter
}

func NewServer(endpointPath string, registry *ToolRegistry, maxInflight int, allowedOrigins []string) *Server {
	return &Server{
		endpointPath:   endpointPath,
		toolRegistry:   registry,
		allowedOrigins: allowedOrigins,
		limiter:        NewInflightLimiter(maxInflight),
	}
}

func (s *Server) Close() {
	// 无状态模式下服务端无需释放长连接或后台 Session
}

// ServeHTTP 处理无状态 MCP 2026-07-28 Streamable HTTP 请求
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 1. HTTP 请求方法校验：只接受 POST，GET 与 DELETE 均返回 405 Method Not Allowed
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, JSONRPCResponse{
			JSONRPC: "2.0",
			Error: &JSONRPCError{
				Code:    ErrCodeMethodNotFound,
				Message: "Method Not Allowed: only POST is supported",
			},
		})
		return
	}

	// 2. DNS Rebinding 防护：Origin 头校验
	origin := r.Header.Get("Origin")
	if !s.isOriginAllowed(origin) {
		writeJSON(w, http.StatusForbidden, JSONRPCResponse{
			JSONRPC: "2.0",
			Error: &JSONRPCError{
				Code:    -32000,
				Message: "Forbidden: origin not allowed",
			},
		})
		return
	}

	// 3. 读取并解析 JSON-RPC 请求 body
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 10<<20)) // 限制 10MB
	if err != nil {
		writeJSON(w, http.StatusBadRequest, JSONRPCResponse{
			JSONRPC: "2.0",
			Error: &JSONRPCError{
				Code:    ErrCodeParseError,
				Message: "Failed to read request body: " + err.Error(),
			},
		})
		return
	}

	var req JSONRPCRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, JSONRPCResponse{
			JSONRPC: "2.0",
			Error: &JSONRPCError{
				Code:    ErrCodeParseError,
				Message: "Parse error: " + err.Error(),
			},
		})
		return
	}

	if req.JSONRPC != "2.0" {
		writeJSON(w, http.StatusBadRequest, JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &JSONRPCError{
				Code:    ErrCodeInvalidRequest,
				Message: "Invalid Request: jsonrpc must be '2.0'",
			},
		})
		return
	}

	// 4. 通知类消息（没有 ID）直接返回 202 Accepted，无需回包
	if req.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// 5. 协议元数据校验 (MCP 2026-07-28 规范要求)
	var paramMeta struct {
		Meta map[string]any `json:"_meta"`
	}
	_ = json.Unmarshal(req.Params, &paramMeta)

	bodyVersion, _ := paramMeta.Meta["io.modelcontextprotocol/protocolVersion"].(string)
	headerVersion := r.Header.Get("MCP-Protocol-Version")

	// 校验 MCP-Protocol-Version 头与 body _meta 字段是否存在且一致
	if headerVersion == "" || bodyVersion == "" || headerVersion != bodyVersion {
		writeJSON(w, http.StatusBadRequest, JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &JSONRPCError{
				Code:    ErrCodeHeaderMismatch,
				Message: "Header mismatch: MCP-Protocol-Version header missing or does not match body _meta",
			},
		})
		return
	}

	// 校验协议版本是否支持
	if headerVersion != ProtocolVersion20260728 {
		writeJSON(w, http.StatusBadRequest, JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &JSONRPCError{
				Code:    ErrCodeUnsupportedProtocolVersion,
				Message: "Unsupported protocol version",
				Data: map[string]any{
					"supported": []string{ProtocolVersion20260728},
					"requested": headerVersion,
				},
			},
		})
		return
	}

	// 校验 Mcp-Method 头与 JSON-RPC method 是否一致
	headerMethod := r.Header.Get("Mcp-Method")
	if headerMethod == "" || headerMethod != req.Method {
		writeJSON(w, http.StatusBadRequest, JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &JSONRPCError{
				Code:    ErrCodeHeaderMismatch,
				Message: "Header mismatch: Mcp-Method header missing or does not match JSON-RPC method",
			},
		})
		return
	}

	// tools/call 要求携带 Mcp-Name 请求头且与 params.name 一致
	if req.Method == "tools/call" {
		var callTool struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(req.Params, &callTool); err != nil || callTool.Name == "" {
			writeJSON(w, http.StatusBadRequest, JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error: &JSONRPCError{
					Code:    ErrCodeInvalidParams,
					Message: "Invalid params: name is required",
				},
			})
			return
		}

		headerName := r.Header.Get("Mcp-Name")
		decodedHeaderName := decodeSentinelHeader(headerName)
		if headerName == "" || decodedHeaderName != callTool.Name {
			writeJSON(w, http.StatusBadRequest, JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error: &JSONRPCError{
					Code:    ErrCodeHeaderMismatch,
					Message: "Header mismatch: Mcp-Name header missing or does not match params.name",
				},
			})
			return
		}
	}

	// 6. 分发执行对应 RPC 方法
	s.dispatch(w, r, req)
}

func (s *Server) dispatch(w http.ResponseWriter, r *http.Request, req JSONRPCRequest) {
	switch req.Method {
	case "server/discover":
		result := DiscoverResult{
			ResultType:        "complete",
			SupportedVersions: []string{ProtocolVersion20260728},
			Capabilities: ServerCapabilities{
				Tools: map[string]any{},
			},
			Instructions: "Read-only tools for the sources mounted on this endpoint. Every call is independent and requires its own arguments.",
			TTLMs:        3600000, // 1 小时缓存新鲜度提示
			CacheScope:   "public",
			Meta: map[string]any{
				"io.modelcontextprotocol/serverInfo": ServerInfo{
					Name:    "echoa",
					Version: "1.0.0",
				},
			},
		}
		writeJSON(w, http.StatusOK, JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  result,
		})

	case "tools/list":
		tools := s.toolRegistry.ListTools()
		result := ListToolsResult{
			ResultType: "complete",
			Tools:      tools,
			TTLMs:      3600000,
			CacheScope: "public",
		}
		writeJSON(w, http.StatusOK, JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  result,
		})

	case "tools/call":
		claims, _ := ClaimsFrom(r.Context())
		principal := extractPrincipal(claims)

		// 检查并发限制槽位
		if !s.limiter.TryAcquire(principal) {
			writeJSON(w, http.StatusTooManyRequests, JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error: &JSONRPCError{
					Code:    ErrCodeTooManyRequests,
					Message: "Too many concurrent requests for principal, please retry later",
				},
			})
			return
		}
		defer s.limiter.Release(principal)

		var callParams CallToolParams
		if err := json.Unmarshal(req.Params, &callParams); err != nil {
			writeJSON(w, http.StatusBadRequest, JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error: &JSONRPCError{
					Code:    ErrCodeInvalidParams,
					Message: "Invalid params: " + err.Error(),
				},
			})
			return
		}

		start := time.Now()
		textResult, err := s.toolRegistry.Execute(r.Context(), claims, callParams.Name, callParams.Arguments)
		elapsed := time.Since(start)

		// 结构化 JSON 审计日志
		auditEntry := map[string]any{
			"timestamp":  time.Now().UTC().Format(time.RFC3339),
			"endpoint":   s.endpointPath,
			"request_id": req.ID,
			"user":       principal,
			"tool":       callParams.Name,
			"args":       sanitizeAuditArgs(callParams.Arguments),
			"latency_ms": elapsed.Milliseconds(),
			"success":    err == nil,
		}
		if err != nil {
			auditEntry["error"] = err.Error()
		}
		if auditJSON, marshalErr := json.Marshal(auditEntry); marshalErr == nil {
			log.Printf("[AUDIT] %s", string(auditJSON))
		}

		if err != nil {
			writeJSON(w, http.StatusOK, JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: CallToolResult{
					ResultType: "complete",
					Content:    []ToolContent{{Type: "text", Text: "Error: " + err.Error()}},
					IsError:    true,
				},
			})
			return
		}

		writeJSON(w, http.StatusOK, JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: CallToolResult{
				ResultType: "complete",
				Content:    []ToolContent{{Type: "text", Text: textResult}},
			},
		})

	default:
		// 规范要求：未实现的方法返回 HTTP 404 及 -32601 Method not found
		writeJSON(w, http.StatusNotFound, JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &JSONRPCError{
				Code:    ErrCodeMethodNotFound,
				Message: "Method not found: " + req.Method,
			},
		})
	}
}

func (s *Server) isOriginAllowed(origin string) bool {
	if origin == "" {
		return true
	}
	if len(s.allowedOrigins) > 0 {
		for _, allowed := range s.allowedOrigins {
			if strings.EqualFold(origin, allowed) {
				return true
			}
		}
		return false
	}

	// 默认仅允许本地回环地址
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	hostname := u.Hostname()
	return hostname == "localhost" || hostname == "127.0.0.1" || hostname == "::1"
}

// decodeSentinelHeader 解析形如 =?base64?<encoded>?= 的 Base64 编码头部值
func decodeSentinelHeader(val string) string {
	if strings.HasPrefix(val, "=?base64?") && strings.HasSuffix(val, "?=") {
		encoded := val[9 : len(val)-2]
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err == nil {
			return string(decoded)
		}
	}
	return val
}

func extractPrincipal(claims *casdoorsdk.Claims) string {
	if claims == nil {
		return "anonymous"
	}
	if claims.Name != "" {
		return claims.Name
	}
	if claims.User.Name != "" {
		return claims.User.Name
	}
	return "anonymous"
}

func sanitizeAuditArgs(args map[string]any) map[string]any {
	if len(args) == 0 {
		return map[string]any{}
	}
	clean := make(map[string]any, len(args))
	sensitiveKeys := map[string]bool{
		"password": true, "secret": true, "token": true,
		"key": true, "auth": true, "credential": true,
	}

	for k, v := range args {
		lowerKey := strings.ToLower(k)
		if sensitiveKeys[lowerKey] {
			clean[k] = "[REDACTED]"
			continue
		}
		switch val := v.(type) {
		case string:
			if len(val) > 200 {
				clean[k] = val[:200] + "... [truncated]"
			} else {
				clean[k] = val
			}
		default:
			clean[k] = v
		}
	}
	return clean
}

// ClaimsFrom 读取鉴权中间件放入请求 context 的 Casdoor 声明。
func ClaimsFrom(ctx context.Context) (*casdoorsdk.Claims, bool) {
	claims, ok := ctx.Value(auth.ClaimsContextKey).(*casdoorsdk.Claims)
	return claims, ok
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

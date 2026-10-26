package mcp

import (
	"encoding/json"
	"log"
	"strings"
	"time"
)

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

func (s *Server) dispatchMessage(sess *Session, req JSONRPCRequest) {
	switch req.Method {
	case "initialize":
		result := InitializeResult{
			ProtocolVersion: "2024-11-05",
			Capabilities: ServerCapabilities{
				Tools: map[string]any{},
			},
			ServerInfo: ServerInfo{
				Name:    "echoa",
				Version: "1.0.0",
			},
		}
		_ = sess.SendJSONRPC(req.ID, result, nil)

	case "notifications/initialized":
		// 握手完成确认，无需回复

	case "ping":
		_ = sess.SendJSONRPC(req.ID, map[string]any{}, nil)

	case "tools/list":
		tools := s.toolRegistry.ListTools()
		_ = sess.SendJSONRPC(req.ID, map[string]any{"tools": tools}, nil)

	case "tools/call":
		var callParams CallToolParams
		if err := json.Unmarshal(req.Params, &callParams); err != nil {
			_ = sess.SendJSONRPC(req.ID, nil, &JSONRPCError{
				Code:    -32602,
				Message: "Invalid params: " + err.Error(),
			})
			return
		}

		start := time.Now()
		textResult, err := s.toolRegistry.Execute(sess.Ctx, sess.Claims, callParams.Name, callParams.Arguments)
		elapsed := time.Since(start)

		// 结构化 JSON 审计日志
		userName := "anonymous"
		if sess.Claims != nil && sess.Claims.Name != "" {
			userName = sess.Claims.Name
		}

		auditEntry := map[string]any{
			"timestamp":  time.Now().UTC().Format(time.RFC3339),
			"session_id": sess.ID,
			"user":       userName,
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
			_ = sess.SendJSONRPC(req.ID, CallToolResult{
				Content: []ToolContent{{Type: "text", Text: "Error: " + err.Error()}},
				IsError: true,
			}, nil)
			return
		}

		_ = sess.SendJSONRPC(req.ID, CallToolResult{
			Content: []ToolContent{{Type: "text", Text: textResult}},
		}, nil)

	default:
		// 如果是通知类消息（没有 ID），则不需要回执
		if req.ID != nil {
			_ = sess.SendJSONRPC(req.ID, nil, &JSONRPCError{
				Code:    -32601,
				Message: "Method not found: " + req.Method,
			})
		}
	}
}

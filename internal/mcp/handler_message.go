package mcp

import (
	"encoding/json"
	"net/http"
)

func (s *Server) HandleMessage(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("sessionId")
	if sessionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"jsonrpc": "2.0",
			"error":   map[string]any{"code": -32600, "message": "Missing sessionId query parameter"},
		})
		return
	}

	session, ok := s.sessionMgr.Get(sessionID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"jsonrpc": "2.0",
			"error":   map[string]any{"code": -32000, "message": "Session not found or expired"},
		})
		return
	}

	// 1. Session 级并发槽位申请（背压保护）
	if !session.TryAcquire() {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"jsonrpc": "2.0",
			"error": map[string]any{
				"code":    -32000,
				"message": "Session busy: maximum concurrent requests reached, please retry later",
			},
		})
		return
	}

	var req JSONRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		session.Release()
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"jsonrpc": "2.0",
			"error":   map[string]any{"code": -32700, "message": "Parse error: " + err.Error()},
		})
		return
	}

	// 2. 异步交由 dispatcher 调度处理，结果通过 SSE Stream 发回；执行完毕后归还槽位
	go func() {
		defer session.Release()
		s.dispatchMessage(session, req)
	}()

	// 按照 MCP 规范，POST 自身直接返回 202 Accepted
	w.WriteHeader(http.StatusAccepted)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

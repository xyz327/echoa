package mcp

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"echoa/internal/auth"
	"github.com/casdoor/casdoor-go-sdk/casdoorsdk"
	"github.com/google/uuid"
)

func (s *Server) resolveEndpointURL(r *http.Request, sessionID string) string {
	// 1. 优先读取反向代理注入的 X-Forwarded-Prefix (如 /mcp 或 /data-mcp)
	if prefix := r.Header.Get("X-Forwarded-Prefix"); prefix != "" {
		prefix = strings.TrimRight(prefix, "/")
		return fmt.Sprintf("%s/message?sessionId=%s", prefix, sessionID)
	}

	// 2. 其次使用配置中的 ResourceURL (若配置了绝对 URL，如 https://mcp.example.com)
	if s.resourceURL != "" {
		trimmed := strings.TrimRight(s.resourceURL, "/")
		return fmt.Sprintf("%s/message?sessionId=%s", trimmed, sessionID)
	}

	// 3. 兜底返回相对路径
	return fmt.Sprintf("/message?sessionId=%s", sessionID)
}

func (s *Server) HandleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	claims, _ := ClaimsFrom(r.Context())

	sessionID := uuid.New().String()
	session := s.sessionMgr.Create(sessionID, claims)
	defer s.sessionMgr.Remove(sessionID)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// 1. 发送 MCP 协议要求的首个 endpoint 事件
	endpointURL := s.resolveEndpointURL(r, sessionID)
	if err := writeSSE(w, flusher, &Event{Event: "endpoint", Data: []byte(endpointURL)}); err != nil {
		return
	}

	// 2. 25 秒心跳保活
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	// 3. 事件消费循环。请求 context 在客户端断开或服务停机时取消。
	for {
		select {
		case <-r.Context().Done():
			return
		case <-session.Ctx.Done():
			return
		case <-ticker.C:
			session.Touch()
			// 注释行作为保活心跳：':' 开头的行不会被客户端解析为事件
			if err := writeSSE(w, flusher, &Event{Comment: "keepalive"}); err != nil {
				return
			}
		case event, ok := <-session.SendChan:
			if !ok {
				return
			}
			if err := writeSSE(w, flusher, event); err != nil {
				return
			}
		}
	}
}

// writeSSE 按 text/event-stream 规范写出一条事件并立即刷出。
// 字段内的换行按规范拆成多行，避免事件被提前截断。
func writeSSE(w http.ResponseWriter, flusher http.Flusher, e *Event) error {
	if e.Comment != "" {
		for _, line := range strings.Split(e.Comment, "\n") {
			if _, err := fmt.Fprintf(w, ": %s\n", line); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprint(w, "\n"); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	if e.Event != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", strings.ReplaceAll(e.Event, "\n", "")); err != nil {
			return err
		}
	}
	for _, line := range strings.Split(string(e.Data), "\n") {
		if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprint(w, "\n"); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// ClaimsFrom 读取鉴权中间件放入请求 context 的 Casdoor 声明。
func ClaimsFrom(ctx context.Context) (*casdoorsdk.Claims, bool) {
	claims, ok := ctx.Value(auth.ClaimsContextKey).(*casdoorsdk.Claims)
	return claims, ok
}

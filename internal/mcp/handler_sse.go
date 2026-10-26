package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/casdoor/casdoor-go-sdk/casdoorsdk"
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/google/uuid"
	"github.com/hertz-contrib/sse"
)

func (s *Server) resolveEndpointURL(c *app.RequestContext, sessionID string) string {
	// 1. 优先读取反向代理注入的 X-Forwarded-Prefix (如 /mcp 或 /data-mcp)
	if prefix := string(c.GetHeader("X-Forwarded-Prefix")); prefix != "" {
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

func (s *Server) HandleSSE(ctx context.Context, c *app.RequestContext) {
	claimsVal, _ := c.Get("claims")
	claims, _ := claimsVal.(*casdoorsdk.Claims)

	sessionID := uuid.New().String()
	session := s.sessionMgr.Create(sessionID, claims)
	defer s.sessionMgr.Remove(sessionID)

	stream := sse.NewStream(c)

	// 1. 发送 MCP 协议要求的首个 endpoint 事件
	endpointURL := s.resolveEndpointURL(c, sessionID)
	err := stream.Publish(&sse.Event{
		Event: "endpoint",
		Data:  []byte(endpointURL),
	})
	if err != nil {
		return
	}

	// 2. 25 秒心跳保活
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	// 3. 事件消费循环
	for {
		select {
		case <-session.Ctx.Done():
			return
		case <-ticker.C:
			session.Touch()
			// 发送注释行作为保活心跳包
			if err := stream.Publish(&sse.Event{Data: []byte(": keepalive\n")}); err != nil {
				return
			}
		case event, ok := <-session.SendChan:
			if !ok {
				return
			}
			if err := stream.Publish(event); err != nil {
				return
			}
		}
	}
}

package mcp

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/utils"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
)

func (s *Server) HandleMessage(ctx context.Context, c *app.RequestContext) {
	sessionID := string(c.Query("sessionId"))
	if sessionID == "" {
		c.JSON(consts.StatusBadRequest, utils.H{
			"jsonrpc": "2.0",
			"error": utils.H{"code": -32600, "message": "Missing sessionId query parameter"},
		})
		return
	}

	session, ok := s.sessionMgr.Get(sessionID)
	if !ok {
		c.JSON(consts.StatusNotFound, utils.H{
			"jsonrpc": "2.0",
			"error": utils.H{"code": -32000, "message": "Session not found or expired"},
		})
		return
	}

	// 1. Session 级并发槽位申请（背压保护）
	if !session.TryAcquire() {
		c.JSON(consts.StatusTooManyRequests, utils.H{
			"jsonrpc": "2.0",
			"error": utils.H{
				"code":    -32000,
				"message": "Session busy: maximum concurrent requests reached, please retry later",
			},
		})
		return
	}

	var req JSONRPCRequest
	if err := c.BindJSON(&req); err != nil {
		session.Release()
		c.JSON(consts.StatusBadRequest, utils.H{
			"jsonrpc": "2.0",
			"error": utils.H{"code": -32700, "message": "Parse error: " + err.Error()},
		})
		return
	}

	// 2. 异步交由 dispatcher 调度处理，结果通过 SSE Stream 发回；执行完毕后归还槽位
	go func() {
		defer session.Release()
		s.dispatchMessage(session, req)
	}()

	// 按照 MCP 规范，POST 自身直接返回 202 Accepted
	c.Status(consts.StatusAccepted)
}

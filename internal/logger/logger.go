package logger

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

type contextKey string

const (
	RequestIDKey contextKey = "request_id"
	EndpointKey  contextKey = "endpoint"
	PrincipalKey contextKey = "principal"
)

// WithRequestID 向 context 中注入 request_id
func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, RequestIDKey, requestID)
}

// WithEndpoint 向 context 中注入 endpoint
func WithEndpoint(ctx context.Context, endpoint string) context.Context {
	return context.WithValue(ctx, EndpointKey, endpoint)
}

// WithPrincipal 向 context 中注入 principal
func WithPrincipal(ctx context.Context, principal string) context.Context {
	return context.WithValue(ctx, PrincipalKey, principal)
}

// contextHandler 自动从 context 提取 request_id, endpoint, principal 注入日志字段
type contextHandler struct {
	slog.Handler
}

func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if ctx != nil {
		if reqID, ok := ctx.Value(RequestIDKey).(string); ok && reqID != "" {
			r.AddAttrs(slog.String("request_id", reqID))
		}
		if ep, ok := ctx.Value(EndpointKey).(string); ok && ep != "" {
			r.AddAttrs(slog.String("endpoint", ep))
		}
		if p, ok := ctx.Value(PrincipalKey).(string); ok && p != "" {
			r.AddAttrs(slog.String("principal", p))
		}
	}
	return h.Handler.Handle(ctx, r)
}

// Init 初始化全局 slog logger
// format 支持 "json" (默认) 或 "text"；level 支持 "debug", "info", "warn", "error"
func Init(levelStr string, format string) {
	var lvl slog.Level
	switch strings.ToLower(strings.TrimSpace(levelStr)) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{
		Level: lvl,
	}

	var baseHandler slog.Handler
	if strings.ToLower(strings.TrimSpace(format)) == "text" {
		baseHandler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		baseHandler = slog.NewJSONHandler(os.Stdout, opts)
	}

	handler := &contextHandler{Handler: baseHandler}
	logger := slog.New(handler)
	slog.SetDefault(logger)
}

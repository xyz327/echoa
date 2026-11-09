package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestContextHandler(t *testing.T) {
	var buf bytes.Buffer
	baseHandler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	handler := &contextHandler{Handler: baseHandler}
	logger := slog.New(handler)

	ctx := context.Background()
	ctx = WithRequestID(ctx, "req-12345")
	ctx = WithEndpoint(ctx, "/mysql-test")
	ctx = WithPrincipal(ctx, "alice")

	logger.InfoContext(ctx, "test message", slog.String("key", "val"))

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log JSON: %v, raw: %s", err, buf.String())
	}

	if entry["msg"] != "test message" {
		t.Errorf("expected msg 'test message', got: %v", entry["msg"])
	}
	if entry["request_id"] != "req-12345" {
		t.Errorf("expected request_id 'req-12345', got: %v", entry["request_id"])
	}
	if entry["endpoint"] != "/mysql-test" {
		t.Errorf("expected endpoint '/mysql-test', got: %v", entry["endpoint"])
	}
	if entry["principal"] != "alice" {
		t.Errorf("expected principal 'alice', got: %v", entry["principal"])
	}
	if entry["key"] != "val" {
		t.Errorf("expected key 'val', got: %v", entry["key"])
	}
}

package datasource

import (
	"context"
	"testing"

	"echoa/internal/config"
	"echoa/internal/mcp"
)

type mockInstance struct {
	pingCalled  bool
	closeCalled bool
}

func (m *mockInstance) Ping(ctx context.Context) error {
	m.pingCalled = true
	return nil
}

func (m *mockInstance) Close() error {
	m.closeCalled = true
	return nil
}

func (m *mockInstance) RegisterTools(registry *mcp.ToolRegistry, sourceName string, qualify bool) {}

func TestDatasourceRegistry(t *testing.T) {
	Register("mock", func(ref config.SourceRef) (Instance, error) {
		return &mockInstance{}, nil
	})

	inst, err := Create(config.SourceRef{Name: "test_mock", Kind: "mock"})
	if err != nil {
		t.Fatalf("unexpected error creating mock datasource: %v", err)
	}

	if err := inst.Ping(context.Background()); err != nil {
		t.Errorf("ping error: %v", err)
	}

	if err := inst.Close(); err != nil {
		t.Errorf("close error: %v", err)
	}

	_, err = Create(config.SourceRef{Name: "unknown", Kind: "unsupported_kind"})
	if err == nil {
		t.Errorf("expected error for unsupported kind, got nil")
	}
}

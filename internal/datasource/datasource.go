package datasource

import (
	"context"
	"fmt"
	"sync"

	"echoa/internal/config"
	"echoa/internal/mcp"
)

// Instance 是每个数据源实例必须实现的统一接口：探活、关闭与向 MCP 注册只读工具。
type Instance interface {
	Ping(ctx context.Context) error
	Close() error
	RegisterTools(registry *mcp.ToolRegistry, sourceName string, qualify bool)
}

// Factory 是创建数据源实例的工厂函数签名。
type Factory func(ref config.SourceRef) (Instance, error)

var (
	registryMu sync.RWMutex
	factories  = make(map[string]Factory)
)

// Register 注册某种类型的数据源工厂 (如 "mysql", "redis")。
func Register(kind string, factory Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	factories[kind] = factory
}

// Create 根据 SourceRef 查找对应种类已注册的工厂并创建数据源实例。
func Create(ref config.SourceRef) (Instance, error) {
	registryMu.RLock()
	factory, ok := factories[ref.Kind]
	registryMu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("unsupported datasource kind %q (registered: %v)", ref.Kind, RegisteredKinds())
	}
	return factory(ref)
}

// RegisteredKinds 返回所有已注册的数据源种类名称列表。
func RegisteredKinds() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	kinds := make([]string, 0, len(factories))
	for k := range factories {
		kinds = append(kinds, k)
	}
	return kinds
}

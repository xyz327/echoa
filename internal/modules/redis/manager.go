package redis

import (
	"fmt"
	"sync"

	"github.com/redis/go-redis/v9"
)

type MultiDBManager struct {
	baseOptions *redis.Options
	allowedDBs  map[int]bool
	mu          sync.RWMutex
	clients     map[int]*redis.Client
}

func NewMultiDBManager(baseOpts *redis.Options, allowedDBs []int) *MultiDBManager {
	dbMap := make(map[int]bool)
	for _, db := range allowedDBs {
		dbMap[db] = true
	}
	// 默认至少允许 DB 0
	if len(dbMap) == 0 {
		dbMap[0] = true
	}

	return &MultiDBManager{
		baseOptions: baseOpts,
		allowedDBs:  dbMap,
		clients:     make(map[int]*redis.Client),
	}
}

// BaseOptions 返回建连参数的副本，供启动探活建一个不影响业务连接池的客户端。
func (m *MultiDBManager) BaseOptions() *redis.Options {
	cp := *m.baseOptions
	return &cp
}

func (m *MultiDBManager) GetClient(db int) (*redis.Client, error) {
	if !m.allowedDBs[db] {
		return nil, fmt.Errorf("access to redis db %d is not permitted by configuration", db)
	}

	m.mu.RLock()
	client, exists := m.clients[db]
	m.mu.RUnlock()
	if exists {
		return client, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if client, exists = m.clients[db]; exists {
		return client, nil
	}

	opts := *m.baseOptions
	opts.DB = db

	newClient := redis.NewClient(&opts)
	m.clients[db] = newClient
	return newClient, nil
}

func (m *MultiDBManager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.clients {
		_ = c.Close()
	}
	return nil
}

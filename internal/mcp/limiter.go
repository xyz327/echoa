package mcp

import "sync"

// InflightLimiter 用于限制单个主体在某 endpoint 上的最大并发正在执行请求数。
// 当计数归零时自动删除 key，防止内存泄露。
type InflightLimiter struct {
	mu       sync.Mutex
	inflight map[string]int
	maxLimit int
}

func NewInflightLimiter(maxLimit int) *InflightLimiter {
	if maxLimit <= 0 {
		maxLimit = 5
	}
	return &InflightLimiter{
		inflight: make(map[string]int),
		maxLimit: maxLimit,
	}
}

// TryAcquire 尝试为 key 获取一个并发槽位，成功返回 true。
func (l *InflightLimiter) TryAcquire(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	current := l.inflight[key]
	if current >= l.maxLimit {
		return false
	}
	l.inflight[key] = current + 1
	return true
}

// Release 释放 key 占用的一个并发槽位。当占用数归零时从 map 中移除。
func (l *InflightLimiter) Release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	current := l.inflight[key]
	if current <= 1 {
		delete(l.inflight, key)
	} else {
		l.inflight[key] = current - 1
	}
}

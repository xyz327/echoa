package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/casdoor/casdoor-go-sdk/casdoorsdk"
	"github.com/hertz-contrib/sse"
)

const (
	// MaxSessionConcurrency 每个 Session 允许的最大并发处理请求数
	MaxSessionConcurrency = 5
	// DefaultSessionTTL Session 默认空闲超时时间（无交互 15 分钟自动清理）
	DefaultSessionTTL = 15 * time.Minute
	// DefaultSendTimeout 向 Session 发送事件的最大超时时间（防慢客户端或假死拖垮 Worker）
	DefaultSendTimeout = 5 * time.Second
)

type Session struct {
	ID       string
	Claims   *casdoorsdk.Claims
	SendChan chan *sse.Event
	Sem      chan struct{} // 并发信号量
	LastSeen time.Time
	mu       sync.Mutex
	Ctx      context.Context
	Cancel   context.CancelFunc
}

func (s *Session) Touch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastSeen = time.Now()
}

func (s *Session) IsIdle(ttl time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.LastSeen) > ttl
}

// TryAcquire 尝试获取并发执行槽位
func (s *Session) TryAcquire() bool {
	select {
	case s.Sem <- struct{}{}:
		s.Touch()
		return true
	default:
		return false
	}
}

// Release 归还并发执行槽位
func (s *Session) Release() {
	select {
	case <-s.Sem:
	default:
	}
}

func (s *Session) SendJSONRPC(id any, result any, rpcErr *JSONRPCError) error {
	return s.SendJSONRPCWithTimeout(id, result, rpcErr, DefaultSendTimeout)
}

func (s *Session) SendJSONRPCWithTimeout(id any, result any, rpcErr *JSONRPCError, timeout time.Duration) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("session send recovered: %v", r)
		}
	}()

	select {
	case <-s.Ctx.Done():
		return errors.New("session closed")
	default:
	}

	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
		Error:   rpcErr,
	}

	data, err := json.Marshal(resp)
	if err != nil {
		return err
	}

	if timeout <= 0 {
		timeout = DefaultSendTimeout
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-s.Ctx.Done():
		return errors.New("session closed")
	case s.SendChan <- &sse.Event{Event: "message", Data: data}:
		return nil
	case <-timer.C:
		return fmt.Errorf("session send timeout after %v: client buffer full or slow receiver", timeout)
	}
}

type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	gcTicker *time.Ticker
	stopGC   chan struct{}
}

func NewSessionManager() *SessionManager {
	mgr := &SessionManager{
		sessions: make(map[string]*Session),
		gcTicker: time.NewTicker(2 * time.Minute),
		stopGC:   make(chan struct{}),
	}
	go mgr.startGC()
	return mgr
}

func (m *SessionManager) startGC() {
	for {
		select {
		case <-m.stopGC:
			return
		case <-m.gcTicker.C:
			m.cleanupExpiredSessions()
		}
	}
}

func (m *SessionManager) cleanupExpiredSessions() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, s := range m.sessions {
		if s.IsIdle(DefaultSessionTTL) {
			s.Cancel()
			delete(m.sessions, id)
		}
	}
}

func (m *SessionManager) Create(id string, claims *casdoorsdk.Claims) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	sess := &Session{
		ID:       id,
		Claims:   claims,
		SendChan: make(chan *sse.Event, 256),
		Sem:      make(chan struct{}, MaxSessionConcurrency),
		LastSeen: time.Now(),
		Ctx:      ctx,
		Cancel:   cancel,
	}
	m.sessions[id] = sess
	return sess
}

func (m *SessionManager) Get(id string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	return s, ok
}

func (m *SessionManager) Remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[id]; ok {
		s.Cancel()
		delete(m.sessions, id)
	}
}

func (m *SessionManager) Close() {
	m.gcTicker.Stop()
	close(m.stopGC)

	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		s.Cancel()
		delete(m.sessions, id)
	}
}

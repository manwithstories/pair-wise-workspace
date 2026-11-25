package model

import (
	"errors"
	"sync"
)

// ErrMemoryLimit 表示读取或缓存会超过 --mem 给定的字节边界。
var ErrMemoryLimit = errors.New("memory limit exceeded")

// MemTracker 是全进程共享的字节预算计数器。
// 所有缓冲区申请都必须经过 Acquire，确保内存峰值受参数边界限制。
type MemTracker struct {
	mu    sync.Mutex
	limit int64
	cur   int64
	peak  int64
}

// NewMemTracker 创建上限为 limit 字节的跟踪器；非正值表示不限。
func NewMemTracker(limit int64) *MemTracker { return &MemTracker{limit: limit} }

// Acquire 申请 n 字节；超过上限时返回 ErrMemoryLimit 且不改变计数。
func (m *MemTracker) Acquire(n int64) error {
	if n < 0 {
		return errors.New("negative allocation")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.limit > 0 && m.cur+n > m.limit {
		return ErrMemoryLimit
	}
	m.cur += n
	if m.cur > m.peak {
		m.peak = m.cur
	}
	return nil
}

// Release 归还 n 字节。
func (m *MemTracker) Release(n int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cur -= n
	if m.cur < 0 {
		m.cur = 0
	}
}

// Peak 返回截至当前的内存峰值（字节）。
func (m *MemTracker) Peak() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.peak
}

// Current 返回当前已占用字节数。
func (m *MemTracker) Current() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur
}

// Limit 返回配置的字节上限。
func (m *MemTracker) Limit() int64 { return m.limit }

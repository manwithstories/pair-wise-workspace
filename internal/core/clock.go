// Package core contains the shared time basis and error model used by the
// parsing, source, timeline, metadata, pipeline and manifest subsystems.
package core

import (
	"sync"
	"time"
)

// Clock is the shared time basis. Production code uses SystemClock; tests and
// deterministic demos use ManualClock so that scheduling never changes results.
type Clock interface {
	Now() time.Time
}

// SystemClock reports wall-clock time.
type SystemClock struct{}

// Now implements Clock.
func (SystemClock) Now() time.Time { return time.Now() }

// ManualClock is a concurrency-safe clock whose value only changes when a test
// or demo explicitly moves it. The zero value is the deterministic epoch.
type ManualClock struct {
	mu sync.Mutex
	t  time.Time
}

// NewManualClock returns a ManualClock frozen at t.
func NewManualClock(t time.Time) *ManualClock {
	return &ManualClock{t: t}
}

// Now implements Clock.
func (c *ManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Set freezes the clock at t.
func (c *ManualClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// Advance moves the clock forward by d.
func (c *ManualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// Epoch is the fixed reference time used by deterministic demos.
var Epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

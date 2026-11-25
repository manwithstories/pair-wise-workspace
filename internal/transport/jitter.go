// Package transport implements RTP-style packet reception: a bounded jitter
// buffer that reorders, de-duplicates and detects sequence wrap, plus a
// single-packet XOR repair layer.
package transport

import (
	"errors"
	"fmt"
)

// Hard caps for the jitter buffer.
const (
	HardMaxBufferBytes = 8 << 20 // 8 MiB total queued payload
	HardMaxReorder     = 1 << 15
	HardMaxLatencyNs   = int64(5 * 1e9)
	HardMaxStreams     = 64
)

// ErrBackpressure is returned when a push would exceed the queue byte cap.
// It is the signal a slow consumer sends upstream to stop producing.
var ErrBackpressure = errors.New("transport: jitter queue byte cap exceeded")

// ErrCanceled is returned by operations on a closed buffer.
var ErrCanceled = errors.New("transport: buffer canceled")

// Config bounds a jitter buffer. Validate rejects unsafe values.
type Config struct {
	MaxBufferBytes int64
	ReorderWindow  int   // max sequence distance accepted
	LatencyNs      int64 // jitter release delay
	GroupSize      int   // XOR group size in packets
}

// DefaultConfig returns the CLI defaults (8 MiB cap, 500 ms jitter).
func DefaultConfig() Config {
	return Config{
		MaxBufferBytes: HardMaxBufferBytes,
		ReorderWindow:  4096,
		LatencyNs:      500 * 1e6,
		GroupSize:      16,
	}
}

// Validate enforces every hard cap before any memory is reserved.
func (c Config) Validate() error {
	if c.MaxBufferBytes <= 0 || c.MaxBufferBytes > HardMaxBufferBytes {
		return fmt.Errorf("transport: buffer cap %d outside (0,%d]", c.MaxBufferBytes, HardMaxBufferBytes)
	}
	if c.ReorderWindow <= 0 || c.ReorderWindow > HardMaxReorder {
		return fmt.Errorf("transport: reorder window %d outside (0,%d]", c.ReorderWindow, HardMaxReorder)
	}
	if c.LatencyNs < 0 || c.LatencyNs > HardMaxLatencyNs {
		return fmt.Errorf("transport: latency %d outside [0,%d]", c.LatencyNs, HardMaxLatencyNs)
	}
	if c.GroupSize < 2 || c.GroupSize > 256 {
		return fmt.Errorf("transport: group size %d outside [2,256]", c.GroupSize)
	}
	return nil
}
